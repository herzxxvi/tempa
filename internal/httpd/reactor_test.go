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
	patient  bool // waiting reads on it have a limit (kernel.patient)
}

type fakeKernel struct {
	t         *testing.T
	sockets   map[int]*socket
	closes    int
	failWatch bool
	reads     int           // calls of read
	awaits    int           // calls of await
	awaitErr  syscall.Errno // returned by the next await instead of reading
	impatient bool          // patient refuses
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
	k.reads++
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
func (k *benchKernel) patient(fd int) bool                   { return true }
func (k *benchKernel) await(fd int, p []byte) (int, syscall.Errno) {
	return k.read(fd, p)
}

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

// The predictive read (loop.chase), still against the scripted kernel. The wait loop's part is
// played by the rig: a request "through the wait" is a wait that reported that one connection, a
// request "through the chase" is one the loop is expected to pick up with its waiting read.

func (k *fakeKernel) patient(fd int) bool {
	s := k.sock(fd)
	if s.patient {
		k.t.Fatalf("descriptor %d was prepared for waiting reads twice", fd)
	}
	if k.impatient {
		return false
	}
	s.patient = true
	return true
}

// await is a read; EAGAIN from it stands for the limit running out.
func (k *fakeKernel) await(fd int, p []byte) (int, syscall.Errno) {
	if s := k.sock(fd); !s.patient {
		k.t.Fatalf("waiting read on descriptor %d, whose waits have no limit", fd)
	}
	k.awaits++
	if k.awaitErr != 0 {
		errno := k.awaitErr
		k.awaitErr = 0
		return 0, errno
	}
	n, errno := k.read(fd, p)
	k.reads--
	return n, errno
}

type rig struct {
	t    *testing.T
	l    *loop
	k    *fakeKernel
	sent map[int]string // by descriptor: every request sent on it
}

func newRig(t *testing.T) *rig {
	l, k := newLoop(t)
	l.enablePredict(0)
	return &rig{t: t, l: l, k: k, sent: map[int]string{}}
}

func (r *rig) put(fd int, request string) {
	s := r.k.sock(fd)
	s.in = append(s.in, request...)
	r.sent[fd] += request
}

// viaWait delivers request on fd as the only event of a wait.
func (r *rig) viaWait(fd int, request string) {
	r.put(fd, request)
	r.l.lone = true
	r.l.ready(r.l.conn(fd))
}

// viaChase delivers request on fd, which must be where the loop expects the next one, to the
// loop's waiting read. It returns what chase returned: whether the loop goes on with another.
func (r *rig) viaChase(fd int, request string) bool {
	r.t.Helper()
	if !r.l.armed() || r.l.prev != fd {
		r.t.Fatalf("the loop is not waiting on descriptor %d (armed %v, expecting %d)", fd, r.l.armed(), r.l.prev)
	}
	r.put(fd, request)
	return r.l.chase()
}

// arm sends requests through the wait, on fds in turn, until the predictive read is armed. It
// returns how many that took.
func (r *rig) arm(fds ...int) int {
	r.t.Helper()
	for n := 0; n <= predictMost+len(fds)+2; n++ {
		if r.l.armed() {
			return n
		}
		r.viaWait(fds[n%len(fds)], req("/arm"+strconv.Itoa(n)))
	}
	r.t.Fatal("the predictive read never armed")
	return 0
}

// check compares what every connection received with what the engine answers to what it sent.
func (r *rig) check() {
	r.t.Helper()
	for fd, sent := range r.sent {
		if got, want := r.k.sockets[fd].out, expected(sent); !bytes.Equal(got, want) {
			r.t.Errorf("descriptor %d received %d bytes, want %d:\n%.300q", fd, len(got), len(want), got)
		}
	}
}

func TestPredictSequentialClient(t *testing.T) {
	r := newRig(t)
	r.k.connect(r.l, 5)
	// The first two exchanges have no "one before last" to match; the streak starts with the third.
	if n := r.arm(5); n != predictAfter+2 {
		t.Errorf("armed after %d exchanges, want %d", n, predictAfter+2)
	}
	reads := r.k.reads
	for i := 0; i < 3*predictCalm; i++ {
		if !r.viaChase(5, req("/s"+strconv.Itoa(i))) {
			t.Fatalf("the loop gave up predicting at request %d", i)
		}
	}
	if r.k.reads != reads || r.k.awaits != 3*predictCalm {
		t.Errorf("%d ordinary reads and %d waiting reads for %d predicted requests", r.k.reads-reads, r.k.awaits, 3*predictCalm)
	}
	if !r.k.sockets[5].patient {
		t.Error("the descriptor was never given a limit for its waiting reads")
	}
	r.check()
}

func TestPredictTwoAlternatingClients(t *testing.T) {
	r := newRig(t)
	r.k.connect(r.l, 5)
	r.k.connect(r.l, 9)
	n := r.arm(5, 9)
	if n != predictAfter+2 {
		t.Errorf("armed after %d exchanges, want %d", n, predictAfter+2)
	}
	fds := [2]int{5, 9}
	for i := n; i < n+200; i++ {
		// Posts of different sizes, so that an answer on the wrong connection cannot look right.
		if !r.viaChase(fds[i%2], post("/a"+strconv.Itoa(i), strings.Repeat("x", i%7))) {
			t.Fatalf("the loop gave up predicting at request %d", i)
		}
	}
	if r.k.awaits != 200 {
		t.Errorf("%d waiting reads, want 200", r.k.awaits)
	}
	r.check()
}

func TestPredictMissBacksOff(t *testing.T) {
	r := newRig(t)
	r.k.connect(r.l, 5)
	r.arm(5)
	// Nothing arrives within the limit.
	if r.l.chase() {
		t.Fatal("chase reports success after a read that found nothing")
	}
	if r.l.armed() || r.l.want != 2*predictAfter {
		t.Fatalf("after a miss: armed %v, streak wanted %d (want false, %d)", r.l.armed(), r.l.want, 2*predictAfter)
	}
	// The request the read missed comes through the wait, and so do the next ones.
	if n := r.arm(5); n != 2*predictAfter {
		t.Errorf("armed again after %d exchanges, want %d", n, 2*predictAfter)
	}
	// Every further miss doubles the requirement, up to a ceiling.
	for i := 0; i < 12; i++ {
		r.arm(5)
		r.l.chase()
	}
	if r.l.want != predictMost {
		t.Errorf("streak wanted after many misses: %d, want %d", r.l.want, predictMost)
	}
	// A long run of hits brings it back.
	r.arm(5)
	for i := 0; i < predictCalm; i++ {
		if !r.viaChase(5, req("/calm"+strconv.Itoa(i))) {
			t.Fatalf("the loop gave up predicting at request %d", i)
		}
	}
	if r.l.want != predictAfter {
		t.Errorf("streak wanted after %d hits: %d, want %d", predictCalm, r.l.want, predictAfter)
	}
	if got := r.k.awaits; got != 1+12+predictCalm {
		t.Errorf("%d waiting reads, want %d", got, 1+12+predictCalm)
	}
	r.check()
}

// An interrupted waiting read says nothing about the guess: the wait takes over for one exchange.
func TestPredictInterrupted(t *testing.T) {
	r := newRig(t)
	r.k.connect(r.l, 5)
	r.arm(5)
	r.put(5, req("/during"))
	r.k.awaitErr = syscall.EINTR
	if r.l.chase() {
		t.Fatal("chase reports success after an interrupted read")
	}
	if !r.l.armed() || r.l.want != predictAfter {
		t.Fatalf("an interruption changed the prediction: armed %v, streak wanted %d", r.l.armed(), r.l.want)
	}
	r.l.lone = true
	r.l.ready(r.l.conn(5)) // the wait finds the request
	if !r.viaChase(5, req("/after")) {
		t.Fatal("the loop stopped predicting after an interruption")
	}
	r.check()
}

// Anything that is not the pattern disarms the read: a request on another connection, two
// connections ready at once, a connection opening, a connection closing.
func TestPredictPatternBreaks(t *testing.T) {
	r := newRig(t)
	r.k.connect(r.l, 5)
	other := r.k.connect(r.l, 9)
	breaks := map[string]func(){
		"a request on another connection": func() { r.viaWait(9, req("/other")) },
		"two connections ready at once": func() {
			r.put(5, req("/both5"))
			r.put(9, req("/both9"))
			r.l.lone = false
			r.l.ready(r.l.conn(5))
			r.l.ready(r.l.conn(9))
		},
		"a new connection": func() { r.k.connect(r.l, 11) },
		"a connection closing": func() {
			other.eof = true
			r.l.lone = true
			r.l.ready(r.l.conn(9))
		},
	}
	for _, name := range []string{"a request on another connection", "two connections ready at once", "a new connection", "a connection closing"} {
		r.arm(5)
		awaits := r.k.awaits
		breaks[name]()
		if r.l.armed() {
			t.Errorf("still armed after %s", name)
		}
		if r.k.awaits != awaits {
			t.Errorf("%s made the loop read ahead", name)
		}
	}
	if !other.closed {
		t.Error("the closed connection was not released")
	}
	delete(r.sent, 9)
	if got, want := other.out, expected(req("/other")+req("/both9")); !bytes.Equal(got, want) {
		t.Errorf("the other connection received %q", got)
	}
	r.check()
}

// What a waiting read can bring besides one whole request: several requests, part of one, the end
// of the connection, an error; and a reply the socket does not take at once.
func TestPredictOtherOutcomes(t *testing.T) {
	r := newRig(t)
	s := r.k.connect(r.l, 5)
	r.arm(5)
	if !r.viaChase(5, req("/one")+post("/two", "body")+req("/three")) {
		t.Error("pipelined requests ended the prediction")
	}
	if s.sends != predictAfter+2+1 {
		t.Errorf("%d sends: the pipelined replies should leave together", s.sends)
	}
	whole := post("/split", "0123456789")
	if r.viaChase(5, whole[:len(whole)-4]) {
		t.Error("chase goes on after reading only part of a request")
	}
	if r.l.armed() {
		t.Error("armed with a request half read")
	}
	r.viaWait(5, whole[len(whole)-4:])
	r.check()

	// A reply larger than the socket takes: the rest follows when it is writable, and only then
	// is the connection read again.
	r.arm(5)
	s.window = 1000
	if r.viaChase(5, req("/bytes/5000")) {
		t.Error("chase goes on with a reply still unsent")
	}
	if !s.writable || r.l.armed() {
		t.Errorf("after a partial write: watching for writability %v, armed %v", s.writable, r.l.armed())
	}
	s.window = -1
	r.put(5, req("/next"))
	r.k.pump(r.l)
	r.check()

	// "Connection: close" through the waiting read.
	r.arm(5)
	if r.viaChase(5, "GET /bye HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n") {
		t.Error("chase goes on after the connection's last request")
	}
	if !s.closed || r.l.armed() {
		t.Errorf("after Connection: close: closed %v, armed %v", s.closed, r.l.armed())
	}
	r.check()

	// The client closes, or the connection fails, while the loop waits on it.
	for fd, end := range map[int]func(*socket){
		7: func(s *socket) { s.eof = true },
		8: func(s *socket) { s.readErr = syscall.ECONNRESET },
	} {
		s := r.k.connect(r.l, fd)
		r.arm(fd)
		end(s)
		if r.l.chase() || !s.closed || r.l.conn(fd) != nil || r.l.armed() {
			t.Errorf("descriptor %d: the connection's end was not handled (closed %v, armed %v)", fd, s.closed, r.l.armed())
		}
	}
	r.check()
}

// Without a limit on the waiting read there is no waiting read.
func TestPredictNeedsALimit(t *testing.T) {
	r := newRig(t)
	r.k.connect(r.l, 5)
	r.k.impatient = true
	r.arm(5)
	r.put(5, req("/first"))
	if r.l.chase() || r.l.predict || r.k.awaits != 0 {
		t.Fatalf("chase went ahead without a limit (predicting %v, %d waiting reads)", r.l.predict, r.k.awaits)
	}
	r.k.pump(r.l)
	for i := 0; i < 2*predictAfter; i++ {
		r.viaWait(5, req("/later"+strconv.Itoa(i)))
	}
	if r.l.armed() || r.k.awaits != 0 {
		t.Error("the predictive read came back")
	}
	r.check()
}

// Without Predict the loop never arms, whatever the traffic looks like.
func TestPredictIsOffByDefault(t *testing.T) {
	l, k := newLoop(t)
	k.connect(l, 5)
	for i := 0; i < 3*predictAfter; i++ {
		k.sockets[5].in = []byte(req("/x"))
		l.lone = true
		l.ready(l.conn(5))
		if l.armed() {
			t.Fatal("armed without Predict")
		}
	}
}

func TestStatsLine(t *testing.T) {
	if line := (&Server{Handler: mirror}).StatsLine(); line != "" {
		t.Errorf("without Count: %q", line)
	}
	srv := &Server{Handler: mirror, Count: true}
	srv.init()
	k := &fakeKernel{t: t, sockets: map[int]*socket{}}
	l := &loop{srv: srv, k: k, stats: srv.stats}
	l.enablePredict(0)
	r := &rig{t: t, l: l, k: k, sent: map[int]string{}}
	k.connect(l, 5)
	k.connect(l, 6)
	k.connect(l, 7)
	k.sockets[7].eof = true
	k.pump(l)          // 7 closes
	l.ready(l.conn(6)) // a wake-up with nothing to read
	n := r.arm(5)
	r.viaChase(5, req("/hit")+req("/pipelined"))
	r.viaChase(5, req("/hit"))
	l.chase() // a miss
	k.connect(l, 8)
	want := "stats: io=netpoll accepted=0 requests=" + strconv.Itoa(n+3) +
		" max_conns=3 read_eagain=1 predict_hits=2 predict_misses=1"
	if line := srv.StatsLine(); line != want {
		t.Errorf("stats line:\n got %s\nwant %s", line, want)
	}
}
