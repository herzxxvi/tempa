// Package httpd is the forge's HTTP/1.1 server engine: a hand-written parser and response writer on
// top of Go's net package, built for an environment where every system call is expensive.
//
// Design (docs/ARCHITECTURE.md has the full picture):
//   - one goroutine per connection, connections are persistent and never closed by the server unless
//     the client asks, the request cannot be parsed, or the socket fails;
//   - a read may deliver several pipelined requests; all of them are answered in order and the
//     responses leave in a single write;
//   - requests are views into the connection's input buffer and responses are appended to its output
//     buffer, so a request that touches only cached data allocates nothing;
//   - there is no per-request logging, no Date header, no compression and no timeouts.
//
// The engine knows nothing about the forge. It calls one Handler; routing below that is the
// Router's job.
package httpd

import (
	"context"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// Handler answers one request by calling exactly one reply method on c (JSON, JSONTail, Bytes,
// Prebuilt or Empty). Everything reachable from c is only valid until the handler returns.
type Handler func(c *Ctx)

// Limits. They are generous for the requests a forge sees (the largest are pushes) and still
// bound what one request can make the server hold: nothing here grows on a client's say-so beyond
// maxRequestSize, and a buffer that grew large is given back as soon as the request is answered.
const (
	inputSize      = 16 << 10                                    // initial input buffer per connection
	outputSize     = 16 << 10                                    // initial output buffer per connection
	maxHeaderSize  = 16 << 10                                    // request line + headers
	maxBodySize    = 32 << 20                                    // request body
	maxRequestSize = maxBodySize + maxBodySize/8 + maxHeaderSize // on the wire; chunked framing included
	growExact      = 1 << 20                                     // beyond this a buffer grows to the size needed, not to twice its size
	keepBuffer     = 1 << 20                                     // a buffer that grew beyond this is replaced once it is empty again
)

// Server is the engine. Set Handler (and Token, for Ctx.Auth) before calling Serve.
type Server struct {
	// Handler receives every request.
	Handler Handler
	// Token is the bearer token Ctx.Auth compares against. Empty accepts any credentials.
	Token string
	// DefaultHost stands in for a missing Host header when handlers build absolute URLs.
	DefaultHost string

	once        sync.Once
	bearer      []byte // "Bearer <Token>", the exact header value the suite's clients send
	defaultHost []byte
	seq         atomic.Uint64
	pool        sync.Pool
}

func (s *Server) init() {
	s.once.Do(func() {
		s.bearer = []byte("Bearer " + s.Token)
		s.defaultHost = []byte(s.DefaultHost)
		s.pool.New = func() any {
			return &Ctx{
				srv:     s,
				in:      make([]byte, inputSize),
				out:     make([]byte, 0, outputSize),
				hdrs:    make([]header, 0, 32),
				extra:   make([]byte, 0, 256),
				Scratch: make([]byte, 0, 4<<10),
			}
		}
	})
}

// Listen opens the listening socket on every interface at an OS-chosen port. It never returns port
// 8080, which belongs to the environment. TCP keep-alive probes are off: they are pointless on the
// links this server runs on and cost a system call per accepted connection.
func Listen() (*net.TCPListener, error) {
	var held []net.Listener
	defer func() {
		for _, l := range held {
			l.Close()
		}
	}()
	for {
		lc := net.ListenConfig{KeepAlive: -1}
		l, err := lc.Listen(context.Background(), "tcp", ":0")
		if err != nil {
			return nil, err
		}
		ln := l.(*net.TCPListener)
		if ln.Addr().(*net.TCPAddr).Port != 8080 {
			return ln, nil
		}
		held = append(held, ln) // keep it bound until we have another port, then release it
	}
}

// Serve accepts connections until the listener is closed.
func (s *Server) Serve(ln *net.TCPListener) error {
	s.init()
	for {
		nc, err := ln.AcceptTCP()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if isClosed(err) {
				return err
			}
			// Out of descriptors or a connection that died in the backlog: wait a moment, keep serving.
			time.Sleep(5 * time.Millisecond)
			continue
		}
		go s.ServeConn(nc)
	}
}

func isClosed(err error) bool {
	if oe, ok := err.(*net.OpError); ok {
		return oe.Err == net.ErrClosed
	}
	return err == net.ErrClosed
}

// ServeConn runs the request loop of one connection and closes it when the loop ends. It is exported
// so tests can drive the engine over any net.Conn.
func (s *Server) ServeConn(nc net.Conn) {
	s.init()
	c := s.pool.Get().(*Ctx)
	c.resetConn()
	defer func() {
		nc.Close()
		c.shrink()
		s.pool.Put(c)
	}()
	last := false // the socket reported EOF or an error together with data: answer it, then stop
	for {
		// Answer every complete request that is buffered. A body that is known to be incomplete is
		// not re-parsed until enough bytes have arrived.
		for c.start < c.end && c.end-c.start >= c.need {
			used, code := c.parse(c.in[c.start:c.end])
			if code == needMore {
				break
			}
			if code != 0 {
				c.replied, c.extra, c.closeAfter = false, c.extra[:0], true
				c.JSON(code, errorBody(code))
				break
			}
			s.dispatch(c)
			c.start += used
			c.scanned, c.need, c.sent100, c.wantContinue = 0, 0, false, false
			if c.closeAfter {
				break
			}
		}
		if len(c.out) > 0 {
			_, err := nc.Write(c.out)
			c.out = c.out[:0]
			if err != nil {
				return
			}
		}
		if c.closeAfter || last {
			return
		}
		if c.wantContinue && !c.sent100 {
			c.sent100 = true
			if _, err := nc.Write(continue100); err != nil {
				return
			}
		}
		// Make room: drop consumed bytes, then grow if the pending request needs more than we have.
		if c.start == c.end {
			c.start, c.end = 0, 0
			c.shrink()
		} else if c.start > 0 {
			c.end = copy(c.in, c.in[c.start:c.end])
			c.start = 0
		}
		if !c.room() {
			// A request that is still incomplete at the size limit (an endless chunked body, a
			// flood of blank lines): refuse it and drop the connection.
			c.replied, c.extra, c.closeAfter = false, c.extra[:0], true
			c.JSON(413, errorBody(413))
			nc.Write(c.out)
			return
		}
		n, err := nc.Read(c.in[c.end:])
		c.end += n
		if err != nil {
			if n == 0 {
				return
			}
			last = true
		}
	}
}

var continue100 = []byte("HTTP/1.1 100 Continue\r\n\r\n")

// dispatch runs the handler for the request just parsed. A panic becomes a 500 reply and the
// connection lives on; a handler that forgets to reply gets a 500 as well.
func (s *Server) dispatch(c *Ctx) {
	mark := len(c.out)
	c.replied = false
	c.extra = c.extra[:0]
	c.nparams = 0
	defer func() {
		if r := recover(); r != nil {
			c.out = c.out[:mark]
			c.replied = false
			c.extra = c.extra[:0]
			c.JSON(500, errorBody(500))
			reportPanic(r)
		}
	}()
	s.Handler(c)
	if !c.replied {
		c.JSON(500, errorBody(500))
	}
}

// reportPanic writes the panic and its stack to stderr. This is the only thing the engine ever logs.
func reportPanic(r any) {
	msg := "httpd: handler panic: "
	switch v := r.(type) {
	case error:
		msg += v.Error()
	case string:
		msg += v
	default:
		msg += "(non-error value)"
	}
	os.Stderr.WriteString(msg + "\n")
	os.Stderr.Write(debug.Stack())
}
