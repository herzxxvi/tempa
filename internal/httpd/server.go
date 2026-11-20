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
	// IO selects the socket I/O loop: IONetpoll (also the meaning of "") or IOEpoll.
	IO string

	once        sync.Once
	bearer      []byte // "Bearer <Token>", the exact header value the suite's clients send
	defaultHost []byte
	seq         atomic.Uint64
	pool        sync.Pool
	stopping    atomic.Bool
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

// The two ways the engine can do its socket I/O. Everything above the sockets (parsing, dispatch,
// response assembly, buffer management) is the same code in both.
const (
	// IONetpoll is the portable default: one goroutine per connection on Go's net package.
	IONetpoll = "netpoll"
	// IOEpoll is a single-threaded, level-triggered epoll loop on raw system calls (Linux only):
	// one wait, one read and one write per exchange, and nothing for the Go scheduler to do.
	IOEpoll = "epoll"
)

// Serve serves connections until the listener is closed (or, for the epoll loop, until Shutdown).
// With IO set to IOEpoll on a platform that has no epoll loop, it serves with IONetpoll.
func (s *Server) Serve(ln *net.TCPListener) error {
	s.init()
	if s.IO == IOEpoll && EpollSupported {
		return s.serveEpoll(ln)
	}
	return s.serveNetpoll(ln)
}

// Shutdown makes Serve return: it closes the listener and, in the epoll loop, every connection.
// The netpoll loop's established connections end when their clients close them.
func (s *Server) Shutdown(ln *net.TCPListener) {
	s.stopping.Store(true)
	ln.Close()
}

func (s *Server) serveNetpoll(ln *net.TCPListener) error {
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

// answer handles every complete request buffered in c and appends the responses to c.out, in
// order. A body that is known to be incomplete is not re-parsed until enough bytes have arrived.
// Afterwards c.closeAfter says whether the connection ends once c.out has been written. This is
// the whole of request handling; the I/O loops only move bytes in and out around it.
func (s *Server) answer(c *Ctx) {
	for c.start < c.end && c.end-c.start >= c.need {
		used, code := c.parse(c.in[c.start:c.end])
		if code == needMore {
			break
		}
		if code != 0 {
			c.refuse(code)
			return
		}
		s.dispatch(c)
		c.start += used
		c.scanned, c.need, c.sent100, c.wantContinue = 0, 0, false, false
		if c.closeAfter {
			return
		}
	}
	if c.wantContinue && !c.sent100 {
		c.sent100 = true
		c.out = append(c.out, continue100...)
	}
}

// refuse answers a request the engine itself rejects and marks the connection for closing.
func (c *Ctx) refuse(status int) {
	c.replied, c.extra, c.closeAfter = false, c.extra[:0], true
	c.JSON(status, errorBody(status))
}

// settle prepares for the next read once c.out has been written: consumed bytes are dropped,
// oversized buffers given back, and room is made for the rest of a pending request. It returns
// false when that request has outgrown the size limit (an endless chunked body, a flood of blank
// lines); a 413 is then waiting in c.out and the connection must close after it.
func (c *Ctx) settle() bool {
	if c.start == c.end {
		c.start, c.end = 0, 0
		c.shrink()
	} else if c.start > 0 {
		c.end = copy(c.in, c.in[c.start:c.end])
		c.start = 0
	}
	if !c.room() {
		c.refuse(413)
		return false
	}
	return true
}

// ServeConn runs the request loop of one connection on the calling goroutine and closes the
// connection when the loop ends. It is the netpoll I/O loop, exported so tests can drive the engine
// over any net.Conn.
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
		s.answer(c)
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
		if !c.settle() {
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
