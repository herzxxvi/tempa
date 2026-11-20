package httpd

import (
	"bytes"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The epoll loop's connection handling (reactor.go) against a scripted kernel. These tests run on
// every platform; the tests in reactor_linux_test.go then run the loop on real sockets.

// socket is one fake connection: what the client has sent and not yet been read, what the server
// has managed to send, and how the kernel answers the next calls.
type socket struct {
	in       []byte        // sent by the client, not yet read by the server
	eof      bool          // the client closed its side (seen once in is drained)
	readErr  syscall.Errno // returned by read once in is drained
	out      []byte        // everything the server sent
	window   int           // bytes the socket still takes before EAGAIN; negative = no limit
	sendErr  syscall.Errno // returned by every send
	maxRead  int           // largest read; 0 = no limit
	watched  bool
	writable bool // what the loop asked to be told about
	closed   bool
	sends    int
}

type fakeKernel struct {
	t         *testing.T
	sockets   map[int]*socket
	closes    int
	failWatch bool
}

func (k *fakeKernel) sock(fd int) *socket {
	s := k.sockets[fd]
	if s == nil || s.closed {
		k.t.Fatalf("system call on descriptor %d, which is not open", fd)
	}
	return s
}

func (k *fakeKernel) read(fd int, p []byte) (int, syscall.Errno) {
	s := k.sock(fd)
	if len(p) == 0 {
		k.t.Fatal("read into an empty buffer")
	}
	if s.writable {
		k.t.Fatalf("descriptor %d was read while the loop waits for it to become writable", fd)
	}
	if len(s.in) == 0 {
		switch {
		case s.readErr != 0:
			return 0, s.readErr
		case s.eof:
			return 0, 0
		}
		return 0, syscall.EAGAIN
	}
	n := len(s.in)
	if s.maxRead > 0 {
		n = min(n, s.maxRead)
	}
	n = copy(p, s.in[:n])
	s.in = s.in[n:]
	return n, 0
}

func (k *fakeKernel) send(fd int, p []byte) (int, syscall.Errno) {
	s := k.sock(fd)
	s.sends++
	if len(p) == 0 {
		k.t.Fatal("send of nothing")
	}
	if s.sendErr != 0 {
		return 0, s.sendErr
	}
	if s.window == 0 {
		return 0, syscall.EAGAIN
	}
	n := len(p)
	if s.window > 0 {
		n = min(n, s.window)
		s.window -= n
	}
	s.out = append(s.out, p[:n]...)
	return n, 0
}

func (k *fakeKernel) watch(fd int, add, writable bool) bool {
	s := k.sock(fd)
	if add == s.watched {
		k.t.Fatalf("descriptor %d: watch add=%v but watched=%v", fd, add, s.watched)
	}
	if k.failWatch {
		return false
	}
	s.watched, s.writable = true, writable
	return true
}

func (k *fakeKernel) close(fd int) {
	k.sock(fd).closed = true
	k.closes++
}

// pump plays the wait loop: while some connection is ready for what the loop watches it for, hand
// it to the loop. It returns how many events were delivered.
func (k *fakeKernel) pump(l *loop) int {
	events := 0
	for progress := true; progress; {
		progress = false
		for fd, s := range k.sockets {
			if s.closed || !s.watched {
				continue
			}
			ready := !s.writable && (len(s.in) > 0 || s.eof || s.readErr != 0)
			ready = ready || (s.writable && (s.window != 0 || s.sendErr != 0))
			if !ready {
				continue
			}
			c := l.conn(fd)
			if c == nil {
				k.t.Fatalf("descriptor %d is open and watched but the loop has no connection for it", fd)
			}
			l.ready(c)
			events++
			progress = true
			if events > 1_000_000 {
				k.t.Fatal("the loop does not settle")
			}
		}
	}
	return events
}

// mirror answers "/bytes/<n>" with n patterned bytes and anything else with the path and body, in
// replies without a request id, so two servers given the same requests send the same bytes.
func mirror(c *Ctx) {
	if rest, ok := strings.CutPrefix(string(c.Path), "/bytes/"); ok {
		n, _ := strconv.Atoi(rest)
		body := make([]byte, n)
		x := uint32(n)
		for i := range body {
			x = x*1664525 + 1013904223
			body[i] = byte('a' + x>>24%26)
		}
		c.Bytes(200, "text/plain", body)
		return
	}
	b := append(c.Scratch[:0], c.Path...)
	b = append(b, '|')
	b = append(b, c.Body...)
	c.Scratch = b
	c.Bytes(200, "text/plain", b)
}

func newLoop(t *testing.T) (*loop, *fakeKernel) {
	srv := &Server{Handler: mirror}
	srv.init()
	k := &fakeKernel{t: t, sockets: map[int]*socket{}}
	return &loop{srv: srv, k: k}, k
}

func (k *fakeKernel) connect(l *loop, fd int) *socket {
	s := &socket{window: -1}
	k.sockets[fd] = s
	l.open(fd)
	return s
}

// expected is what a connection that sent requests must have received: the same engine, without
// any I/O loop.
func expected(requests string) []byte {
	return bytes.Clone(NewTester(&Server{Handler: mirror}).Do([]byte(requests)))
}

func req(path string) string { return "GET " + path + " HTTP/1.1\r\nHost: h\r\n\r\n" }

func post(path, body string) string {
	return "POST " + path + " HTTP/1.1\r\nHost: h\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

func TestLoopExchangesAndPipelining(t *testing.T) {
	l, k := newLoop(t)
	s := k.connect(l, 7)
	if !s.watched || s.writable {
		t.Fatal("a new connection must be watched for requests")
	}
	// One request, one read, one send.
	s.in = []byte(req("/one"))
	if events := k.pump(l); events != 1 || s.sends != 1 || !bytes.Equal(s.out, expected(req("/one"))) {
		t.Fatalf("single exchange: %d events, %d sends, %q", events, s.sends, s.out)
	}
	// Three pipelined requests: answered in order, in one send.
	s.out, s.sends = nil, 0
	batch := req("/a") + post("/b", "body of b") + req("/c")
	s.in = []byte(batch)
	if k.pump(l); s.sends != 1 || !bytes.Equal(s.out, expected(batch)) {
		t.Fatalf("pipelined batch: %d sends, %q", s.sends, s.out)
	}
	// A request that arrives a few bytes per read.
	s.out, s.sends, s.maxRead = nil, 0, 5
	slow := post("/slow", strings.Repeat("0123456789", 40))
	s.in = []byte(slow)
	if k.pump(l); s.sends != 1 || !bytes.Equal(s.out, expected(slow)) {
		t.Fatalf("request in small reads: %d sends, %q", s.sends, s.out)
	}
	// A body far larger than the input buffer.
	s.out, s.maxRead = nil, 0
	large := post("/large", strings.Repeat("x", 3*inputSize+17)) + req("/after")
	s.in = []byte(large)
	if k.pump(l); !bytes.Equal(s.out, expected(large)) {
		t.Fatalf("large request: %d bytes sent, want %d", len(s.out), len(expected(large)))
	}
	if s.closed || k.closes != 0 {
		t.Fatal("the connection must stay open")
	}
}

func TestLoopPartialWrites(t *testing.T) {
	l, k := newLoop(t)
	s := k.connect(l, 3)
	requests := req("/bytes/200000") + req("/tail")
	want := expected(requests)
	s.window = 1000 // the socket takes 1000 bytes and then no more
	s.in = []byte(requests)
	k.pump(l)
	if len(s.out) != 1000 || !s.writable {
		t.Fatalf("after a short write: %d bytes out, waiting for writability: %v", len(s.out), s.writable)
	}
	// While it waits to write, the loop must not read: a new request stays in the socket.
	later := req("/later")
	s.in = []byte(later)
	if events := k.pump(l); events != 0 || len(s.in) != len(later) {
		t.Fatalf("a blocked connection was read (%d events, %d bytes left)", events, len(s.in))
	}
	// The client reads in irregular steps; every step lets a little more through.
	for step := 0; len(s.out) < len(want); step++ {
		if !s.writable {
			t.Fatalf("stopped waiting for writability with %d of %d bytes sent", len(s.out), len(want))
		}
		s.window = 1 + step*977%9000
		if k.pump(l) == 0 {
			t.Fatal("writable connection was not served")
		}
		if step > 10000 {
			t.Fatal("the response never completes")
		}
	}
	// Drained: back to reading, and the request that waited is answered next.
	s.window = -1
	k.pump(l)
	if s.writable {
		t.Fatal("after draining the connection must wait for requests again")
	}
	if !bytes.Equal(s.out, append(want, expected(later)...)) {
		t.Fatalf("bytes sent differ from the expected stream (%d bytes, want %d)", len(s.out), len(want)+len(expected(later)))
	}
}

func TestLoopConnectionEndings(t *testing.T) {
	l, k := newLoop(t)

	// The client closes after its request: answered, then closed.
	s := k.connect(l, 4)
	s.in, s.eof = []byte(req("/bye")), true
	k.pump(l)
	if !bytes.Equal(s.out, expected(req("/bye"))) || !s.closed || l.conn(4) != nil {
		t.Fatalf("request then close: %q, closed %v", s.out, s.closed)
	}
	// The client closes in the middle of a request: closed, nothing sent.
	s = k.connect(l, 5)
	s.in, s.eof = []byte("GET /partial HTTP/1.1\r\nHo"), true
	if k.pump(l); len(s.out) != 0 || !s.closed {
		t.Fatalf("close mid-request: %q, closed %v", s.out, s.closed)
	}
	// A read error closes.
	s = k.connect(l, 6)
	s.readErr = syscall.ECONNRESET
	if k.pump(l); !s.closed {
		t.Fatal("a read error must close the connection")
	}
	// A send error closes, also in the middle of a response the socket had stopped taking.
	s = k.connect(l, 8)
	s.in, s.window = []byte(req("/bytes/50000")), 100
	k.pump(l)
	s.sendErr = syscall.EPIPE
	if k.pump(l); !s.closed || len(s.out) != 100 {
		t.Fatalf("send error while blocked: closed %v, %d bytes", s.closed, len(s.out))
	}
	// "Connection: close": closed after the response, even when the response had to wait.
	s = k.connect(l, 9)
	closing := "GET /bytes/30000 HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n"
	s.in, s.window = []byte(closing), 10
	k.pump(l)
	if s.closed {
		t.Fatal("closed before the response was out")
	}
	s.window = -1
	if k.pump(l); !s.closed || !bytes.Equal(s.out, expected(closing)) {
		t.Fatalf("Connection: close: closed %v, %d bytes", s.closed, len(s.out))
	}
	// A request the engine refuses: answered with the error, then closed.
	s = k.connect(l, 10)
	s.in = []byte("POST / HTTP/1.1\r\nHost: h\r\nContent-Length: " + strconv.Itoa(maxBodySize+1) + "\r\n\r\n")
	if k.pump(l); !s.closed || !bytes.HasPrefix(s.out, []byte("HTTP/1.1 413 ")) {
		t.Fatalf("oversized request: closed %v, %q", s.closed, s.out)
	}
	// Interrupted and would-block calls change nothing.
	s = k.connect(l, 11)
	s.readErr = syscall.EINTR
	if l.ready(l.conn(11)); s.closed {
		t.Fatal("an interrupted read closed the connection")
	}
	s.readErr = 0
	if l.ready(l.conn(11)); s.closed {
		t.Fatal("a read that would block closed the connection")
	}
	if k.closes != 6 {
		t.Fatalf("%d closes, want 6", k.closes)
	}

	// A descriptor number is reused: the new connection starts clean, whatever the old one left.
	s = k.connect(l, 8)
	s.in = []byte(req("/reused"))
	if k.pump(l); !bytes.Equal(s.out, expected(req("/reused"))) || s.closed {
		t.Fatalf("reused descriptor: %q", s.out)
	}
	// A descriptor the kernel will not watch is closed at once; one out of range is refused.
	k.failWatch = true
	if s = k.connect(l, 12); !s.closed || l.conn(12) != nil {
		t.Fatal("a connection that cannot be watched must be closed")
	}
	k.failWatch = false
	if s = k.connect(l, maxDescriptor); !s.closed {
		t.Fatal("a descriptor beyond the table limit must be closed")
	}
	if l.conn(-1) != nil || l.conn(99999) != nil {
		t.Fatal("conn of an unknown descriptor")
	}
	l.endAll()
	for fd, s := range k.sockets {
		if !s.closed {
			t.Errorf("descriptor %d left open by endAll", fd)
		}
	}
}

// Many connections, their requests arriving in arbitrary pieces and their sockets taking arbitrary
// amounts, in arbitrary order: each must end up with exactly the bytes the engine produces for its
// request stream.
func TestLoopManyConnectionsInterleaved(t *testing.T) {
	l, k := newLoop(t)
	x := uint32(12345)
	rnd := func(n int) int {
		x = x*1664525 + 1013904223
		return int(x>>8) % n
	}
	const connections = 60
	pending := make([]string, connections) // not yet handed to the fake socket
	want := make([][]byte, connections)
	for i := 0; i < connections; i++ {
		var stream strings.Builder
		for r := 0; r < 4+rnd(8); r++ {
			switch rnd(4) {
			case 0:
				stream.WriteString(req("/c" + strconv.Itoa(i) + "/r" + strconv.Itoa(r)))
			case 1:
				stream.WriteString(post("/c"+strconv.Itoa(i)+"/p"+strconv.Itoa(r), strings.Repeat("b", rnd(40000))))
			case 2:
				stream.WriteString(req("/bytes/" + strconv.Itoa(rnd(300000))))
			case 3:
				stream.WriteString(req("/x") + req("/y") + req("/z"))
			}
		}
		pending[i] = stream.String()
		want[i] = expected(pending[i])
		k.connect(l, 100+i).window = 0
	}
	for step := 0; ; step++ {
		done := true
		for i := 0; i < connections; i++ {
			s := k.sockets[100+i]
			if len(pending[i]) > 0 && rnd(3) == 0 {
				n := min(len(pending[i]), 1+rnd(30000))
				s.in = append(s.in, pending[i][:n]...)
				pending[i] = pending[i][n:]
			}
			if rnd(2) == 0 {
				s.window = rnd(60000)
			}
			if len(pending[i]) > 0 || len(s.out) < len(want[i]) {
				done = false
			}
		}
		k.pump(l)
		if done {
			break
		}
		if step > 200000 {
			t.Fatal("the connections never finish")
		}
	}
	for i := 0; i < connections; i++ {
		s := k.sockets[100+i]
		if !bytes.Equal(s.out, want[i]) {
			t.Fatalf("connection %d: %d bytes sent, want %d (or different bytes)", i, len(s.out), len(want[i]))
		}
		if s.closed || s.writable {
			t.Fatalf("connection %d: closed %v, still waiting to write %v", i, s.closed, s.writable)
		}
	}
	if k.closes != 0 {
		t.Fatalf("%d connections were closed", k.closes)
	}
}

// A request that never completes is refused when it reaches the size limit: the loop answers 413
// and closes, instead of buffering without end.
func TestLoopEndlessRequest(t *testing.T) {
	l, k := newLoop(t)
	s := k.connect(l, 21)
	s.in = []byte("POST /endless HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n")
	k.pump(l)
	chunk := []byte("100000\r\n" + strings.Repeat("x", 1<<20) + "\r\n")
	fed := 0
	for !s.closed {
		if fed > 2*maxRequestSize {
			t.Fatalf("%d bytes of an endless request were accepted", fed)
		}
		s.in = append(s.in[:0], chunk...)
		fed += len(chunk)
		k.pump(l)
	}
	if !bytes.HasPrefix(s.out, []byte("HTTP/1.1 413 ")) || !bytes.Contains(s.out, []byte("Connection: close\r\n")) {
		t.Fatalf("endless request: %q", s.out)
	}
	if fed < maxBodySize {
		t.Fatalf("refused after only %d bytes", fed)
	}
}

// benchKernel serves the same request over and over and discards what is sent.
type benchKernel struct {
	request []byte
	fresh   bool
	sent    int
}

func (k *benchKernel) read(fd int, p []byte) (int, syscall.Errno) {
	if !k.fresh {
		return 0, syscall.EAGAIN
	}
	k.fresh = false
	return copy(p, k.request), 0
}

func (k *benchKernel) send(fd int, p []byte) (int, syscall.Errno) {
	k.sent += len(p)
	return len(p), 0
}

func (k *benchKernel) watch(fd int, add, writable bool) bool { return true }
func (k *benchKernel) close(fd int)                          {}

// One exchange through the epoll loop's connection handling: one read, one send, no allocation.
func BenchmarkLoopExchange(b *testing.B) {
	tail := AppendTail(nil, bytes.Repeat([]byte("x"), 700))
	srv := &Server{Token: "0123456789abcdef0123456789abcdef", Handler: func(c *Ctx) {
		if c.Auth() != AuthOK {
			c.JSON(401, []byte("{}"))
			return
		}
		c.JSONTail(200, tail)
	}}
	srv.init()
	k := &benchKernel{request: []byte("GET /api/v3/repos/adaiversen/bench-read/issues/1 HTTP/1.1\r\nhost: 127.0.0.1:43210\r\nconnection: keep-alive\r\n" +
		"accept: application/vnd.github+json\r\nauthorization: Bearer 0123456789abcdef0123456789abcdef\r\nuser-agent: node\r\n" +
		"accept-language: *\r\nsec-fetch-mode: cors\r\naccept-encoding: gzip, deflate\r\n\r\n")}
	l := &loop{srv: srv, k: k}
	l.open(5)
	c := l.conn(5)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k.fresh = true
		l.ready(c)
	}
	if k.sent < 700*b.N {
		b.Fatalf("sent %d bytes in %d exchanges", k.sent, b.N)
	}
}
