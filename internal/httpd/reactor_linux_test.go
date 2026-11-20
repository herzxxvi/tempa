//go:build linux

package httpd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The epoll loop is tested twice over: every engine test of this package runs again with the server
// on the loop, and the tests below go after what only the loop has to get right (writes the socket
// does not take, many connections on one thread, descriptors closed and reused, living with one P).

// TestEpollRunsTheEngineTests repeats the socket tests of httpd_test.go on the epoll loop.
func TestEpollRunsTheEngineTests(t *testing.T) {
	testIO = IOEpoll
	defer func() { testIO = IONetpoll }()
	for name, test := range engineTests() {
		t.Run(name, test)
	}
}

func engineTests() map[string]func(*testing.T) {
	return map[string]func(*testing.T){
		"KeepAliveAndHeaders":                  TestKeepAliveAndHeaders,
		"RequestIDsAreUnique":                  TestRequestIDsAreUnique,
		"PipelinedRequestsAnswerInOrder":       TestPipelinedRequestsAnswerInOrder,
		"BodyArrivingInPieces":                 TestBodyArrivingInPieces,
		"LargeBodyGrowsTheBuffer":              TestLargeBodyGrowsTheBuffer,
		"ChunkedBody":                          TestChunkedBody,
		"ChunkedBodyInManyPieces":              TestChunkedBodyInManyPieces,
		"ExpectContinue":                       TestExpectContinue,
		"ConnectionCloseAndHTTP10":             TestConnectionCloseAndHTTP10,
		"MalformedRequests":                    TestMalformedRequests,
		"PanicBecomes500AndConnectionSurvives": TestPanicBecomes500AndConnectionSurvives,
		"OtherReplyKinds":                      TestOtherReplyKinds,
		"TailReplies":                          TestTailReplies,
		"Auth":                                 TestAuth,
		"HostileHostIsReplaced":                TestHostileHostIsReplaced,
		"BufferGrowthIsBounded":                TestBufferGrowthIsBounded,
	}
}

// epollServer starts a server on the epoll loop and returns its address.
func epollServer(t *testing.T, h Handler) string {
	t.Helper()
	return runEpoll(t, &Server{Handler: h, Token: "secret", DefaultHost: "fallback:1", IO: IOEpoll})
}

// runEpoll starts srv, which must be set up for the epoll loop, and returns its address.
func runEpoll(t *testing.T, srv *Server) string {
	t.Helper()
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Shutdown(ln)
		select {
		case err := <-stopped:
			if !errors.Is(err, net.ErrClosed) {
				t.Errorf("the epoll loop ended with %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the epoll loop did not stop after Shutdown")
		}
	})
	return addr
}

func dial(t *testing.T, addr string) (*net.TCPConn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	return conn.(*net.TCPConn), bufio.NewReaderSize(conn, 64<<10)
}

// readResponse reads one response; unlike client.read it reports errors instead of failing the
// test, so it can be used from any goroutine.
func readResponse(r *bufio.Reader) (status int, body []byte, err error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	if len(line) < 12 || !strings.HasPrefix(line, "HTTP/1.1 ") {
		return 0, nil, fmt.Errorf("bad status line %q", line)
	}
	status, _ = strconv.Atoi(line[9:12])
	length := 0
	for {
		if line, err = r.ReadString('\n'); err != nil {
			return 0, nil, err
		}
		if line == "\r\n" {
			break
		}
		if name, value, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), ": "); strings.EqualFold(name, "content-length") {
			length, _ = strconv.Atoi(value)
		}
	}
	body = make([]byte, length)
	_, err = io.ReadFull(r, body)
	return status, body, err
}

// pattern is a body whose every byte depends on its position, so a lost, repeated or reordered
// stretch of a large response cannot go unnoticed.
func pattern(n int) []byte {
	b := make([]byte, n)
	x := uint32(n)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// sizeHandler answers "/bytes/<n>" with pattern(n) and anything else with the path and body echoed.
func sizeHandler(count *atomic.Int64) Handler {
	cache := map[int][]byte{}
	var mu sync.Mutex
	return func(c *Ctx) {
		if count != nil {
			count.Add(1)
		}
		if rest, ok := strings.CutPrefix(string(c.Path), "/bytes/"); ok {
			n, _ := strconv.Atoi(rest)
			mu.Lock()
			body := cache[n]
			if body == nil {
				body = pattern(n)
				cache[n] = body
			}
			mu.Unlock()
			c.Bytes(200, "application/octet-stream", body)
			return
		}
		b := append(c.Scratch[:0], c.Path...)
		b = append(b, '|')
		b = append(b, c.Body...)
		c.Scratch = b
		c.JSON(200, b)
	}
}

const get = " HTTP/1.1\r\nHost: h\r\n\r\n"

// A response far larger than the socket buffer, to a client that is not reading: the loop must
// keep the rest, wait for the socket to drain, serve everyone else meanwhile, and deliver every
// byte in order once the client reads. Requests pipelined behind the large one are answered after
// it, in order.
func TestEpollPartialWrites(t *testing.T) {
	addr := epollServer(t, sizeHandler(nil))
	const big = 24 << 20
	slow, slowReader := dial(t, addr)
	// Keep the kernel from absorbing much of it, but no smaller than one loopback segment (64 KiB):
	// below that, Linux falls into zero-window pacing at about 150 kB a second and 24 MiB take
	// minutes, whichever loop serves them.
	slow.SetReadBuffer(64 << 10)
	if _, err := io.WriteString(slow, "GET /bytes/"+strconv.Itoa(big)+get+"GET /after-1"+get+"GET /bytes/70000"+get+"GET /after-2"+get); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the server is now stuck mid-response on this connection

	// Other connections are served while that one is blocked.
	for i := 0; i < 20; i++ {
		other, r := dial(t, addr)
		io.WriteString(other, "GET /other/"+strconv.Itoa(i)+get)
		if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != "/other/"+strconv.Itoa(i)+"|" {
			t.Fatalf("connection %d while another is blocked: %d %q %v", i, status, body, err)
		}
		other.Close()
	}

	// Now read the slow connection. The server finishes the response in many partial writes, as
	// the socket makes room.
	want := pattern(big)
	status, body, err := readResponse(slowReader)
	if err != nil || status != 200 || len(body) != big {
		t.Fatalf("large response: %d, %d bytes, %v", status, len(body), err)
	}
	for i := range body {
		if body[i] != want[i] {
			t.Fatalf("large response differs at byte %d", i)
		}
	}
	for _, expect := range []string{"/after-1|", string(pattern(70000)), "/after-2|"} {
		status, body, err := readResponse(slowReader)
		if err != nil || status != 200 || string(body) != expect {
			t.Fatalf("pipelined response after the large one: %d, %d bytes, %v", status, len(body), err)
		}
	}
	// And the connection is back to normal.
	io.WriteString(slow, "POST /again HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\n\r\nhello")
	if status, body, err := readResponse(slowReader); err != nil || status != 200 || string(body) != "/again|hello" {
		t.Fatalf("request after the large response: %d %q %v", status, body, err)
	}
}

// Many connections at once, each with its own sequence of requests (single, pipelined, with
// bodies, large and small answers): every response must be the right one for its request, and the
// handler must have run exactly once per request.
func TestEpollManyConnections(t *testing.T) {
	var handled atomic.Int64
	addr := epollServer(t, sizeHandler(&handled))
	const connections, rounds = 200, 12
	var wg sync.WaitGroup
	failures := make(chan error, connections)
	for id := 0; id < connections; id++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				failures <- err
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(60 * time.Second))
			r := bufio.NewReader(conn)
			for round := 0; round < rounds; round++ {
				tag := fmt.Sprintf("/c%d/r%d", id, round)
				switch round % 4 {
				case 0: // one request
					io.WriteString(conn, "GET "+tag+get)
					if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != tag+"|" {
						failures <- fmt.Errorf("%s: %d %q %v", tag, status, body, err)
						return
					}
				case 1: // three pipelined in one write
					io.WriteString(conn, "GET "+tag+"/a"+get+"GET "+tag+"/b"+get+"GET "+tag+"/c"+get)
					for _, part := range []string{"/a", "/b", "/c"} {
						if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != tag+part+"|" {
							failures <- fmt.Errorf("%s%s: %d %q %v", tag, part, status, body, err)
							return
						}
					}
				case 2: // a body sent in two pieces
					payload := strings.Repeat(strconv.Itoa(id%10), 3000+id)
					io.WriteString(conn, "POST "+tag+" HTTP/1.1\r\nHost: h\r\nContent-Length: "+strconv.Itoa(len(payload))+"\r\n\r\n"+payload[:100])
					time.Sleep(time.Millisecond)
					io.WriteString(conn, payload[100:])
					if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != tag+"|"+payload {
						failures <- fmt.Errorf("%s: %d, %d bytes, %v", tag, status, len(body), err)
						return
					}
				case 3: // a response of a few hundred kilobytes
					size := 200000 + id*37
					io.WriteString(conn, "GET /bytes/"+strconv.Itoa(size)+get)
					status, body, err := readResponse(r)
					if err != nil || status != 200 || len(body) != size || string(body[:64]) != string(pattern(size)[:64]) {
						failures <- fmt.Errorf("%s: %d, %d bytes, %v", tag, status, len(body), err)
						return
					}
				}
			}
		}(id)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if want := int64(connections * rounds / 4 * 6); handled.Load() != want {
		t.Errorf("the handler ran %d times, want %d", handled.Load(), want)
	}
}

func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	return len(entries)
}

// Connections that end in every possible way: closed between requests, closed mid-request,
// half-closed after a request, reset while the server is writing to them. The loop must answer what
// was complete, release every descriptor, and keep serving.
func TestEpollConnectionEndings(t *testing.T) {
	addr := epollServer(t, sizeHandler(nil))
	// Warm up, so the pool and the tables have their size before descriptors are counted.
	for i := 0; i < 5; i++ {
		conn, r := dial(t, addr)
		io.WriteString(conn, "GET /warm"+get)
		readResponse(r)
		conn.Close()
	}
	time.Sleep(50 * time.Millisecond)
	before := openDescriptors(t)

	for i := 0; i < 300; i++ {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		tcp := conn.(*net.TCPConn)
		tcp.SetDeadline(time.Now().Add(20 * time.Second))
		r := bufio.NewReader(conn)
		switch i % 5 {
		case 0: // request, response, close
			io.WriteString(conn, "GET /x"+get)
			if status, _, err := readResponse(r); err != nil || status != 200 {
				t.Fatalf("plain exchange %d: %d %v", i, status, err)
			}
		case 1: // closed in the middle of the headers
			io.WriteString(conn, "GET /partial HTTP/1.1\r\nHo")
		case 2: // half-close: the response must still arrive, then the server closes
			io.WriteString(conn, "GET /half"+get)
			tcp.CloseWrite()
			if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != "/half|" {
				t.Fatalf("half-closed exchange %d: %d %q %v", i, status, body, err)
			}
			if _, err := r.ReadByte(); err != io.EOF {
				t.Fatalf("after a half-close the server should close, got %v", err)
			}
		case 3: // reset while a large response is on its way
			io.WriteString(conn, "GET /bytes/8000000"+get)
			time.Sleep(time.Millisecond)
			tcp.SetLinger(0)
		case 4: // connect and leave
		}
		conn.Close()
	}
	// Everything is released, and the server still answers.
	deadline := time.Now().Add(5 * time.Second)
	for openDescriptors(t) > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := openDescriptors(t); after > before {
		t.Errorf("descriptors: %d before, %d after 300 connections", before, after)
	}
	conn, r := dial(t, addr)
	io.WriteString(conn, "GET /still-here"+get)
	if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != "/still-here|" {
		t.Fatalf("after the endings: %d %q %v", status, body, err)
	}
}

// A request dribbled in a byte at a time is put together across many events.
func TestEpollSlowClient(t *testing.T) {
	addr := epollServer(t, sizeHandler(nil))
	conn, r := dial(t, addr)
	request := "POST /slow HTTP/1.1\r\nHost: h\r\nContent-Length: 11\r\n\r\nhello world"
	for i := 0; i < len(request); i++ {
		if _, err := io.WriteString(conn, request[i:i+1]); err != nil {
			t.Fatal(err)
		}
		if i%7 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != "/slow|hello world" {
		t.Fatalf("dribbled request: %d %q %v", status, body, err)
	}
}

// The server's own configuration: one P. The loop holds it between yields, so this checks that
// other goroutines (here: the test's clients and a timer) still get to run, and that the loop
// yields on its own when it is busy.
func TestEpollWithOneP(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var handled atomic.Int64
	addr := epollServer(t, sizeHandler(&handled))
	ticks := make(chan struct{}, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		ticks <- struct{}{}
	}()
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for id := 0; id < 8; id++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				failures <- err
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(60 * time.Second))
			r := bufio.NewReader(conn)
			for round := 0; round < 25; round++ {
				tag := fmt.Sprintf("/p%d/r%d", id, round)
				io.WriteString(conn, "GET "+tag+get)
				if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != tag+"|" {
					failures <- fmt.Errorf("%s: %d %q %v", tag, status, body, err)
					return
				}
			}
		}(id)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	select {
	case <-ticks:
	case <-time.After(5 * time.Second):
		t.Error("a timer in another goroutine never fired while the loop was running")
	}
	if handled.Load() != 200 {
		t.Errorf("the handler ran %d times, want 200", handled.Load())
	}
}

// The predictive read (Server.Predict) on real sockets: accepted sockets in blocking mode, reads
// that wait with a limit, and everything else exactly as before.

// predictServer starts a server on the epoll loop with predictive reads and counters.
func predictServer(t *testing.T, h Handler) (string, *Server) {
	t.Helper()
	srv := &Server{Handler: h, Token: "secret", DefaultHost: "fallback:1", IO: IOEpoll, Predict: true, Count: true}
	return runEpoll(t, srv), srv
}

// twoPs makes sure the test's clients can run while the loop sits in a waiting read: in one
// process with one P they could not, and every such read would run into its limit.
func twoPs(t *testing.T) {
	t.Helper()
	if runtime.NumCPU() < 2 {
		t.Skip("needs two CPUs: the client must run while the server waits in a read")
	}
	if runtime.GOMAXPROCS(0) < 2 {
		before := runtime.GOMAXPROCS(2)
		t.Cleanup(func() { runtime.GOMAXPROCS(before) })
	}
}

// exchange sends one GET for tag on conn and checks the reply.
func exchange(conn net.Conn, r *bufio.Reader, tag string) error {
	if _, err := io.WriteString(conn, "GET "+tag+get); err != nil {
		return err
	}
	if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != tag+"|" {
		return fmt.Errorf("%s: %d %q %v", tag, status, body, err)
	}
	return nil
}

// TestPredictRunsTheEngineTests repeats the socket tests of httpd_test.go with every experiment of
// the loop switched on, and with the predictive read armed after two exchanges instead of 32, so
// that requests in pieces, chunked bodies, pipelines and closes all meet the waiting read.
func TestPredictRunsTheEngineTests(t *testing.T) {
	twoPs(t)
	testIO, testTune = IOEpoll, func(srv *Server) {
		srv.Predict, srv.DeferAccept, srv.Count, srv.predictAfter = true, true, true, 2
	}
	defer func() { testIO, testTune = IONetpoll, nil }()
	for name, test := range engineTests() {
		t.Run(name, test)
	}
}

func TestPredictSequentialClientOnSockets(t *testing.T) {
	twoPs(t)
	var handled atomic.Int64
	addr, srv := predictServer(t, sizeHandler(&handled))
	conn, r := dial(t, addr)
	const rounds = 2000
	for i := 0; i < rounds; i++ {
		// Replies of different sizes now and then, so a repeated or misplaced one cannot pass.
		if i%100 == 99 {
			io.WriteString(conn, "GET /bytes/"+strconv.Itoa(1000+i)+get)
			if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != string(pattern(1000+i)) {
				t.Fatalf("round %d: %d, %d bytes, %v", i, status, len(body), err)
			}
			continue
		}
		if err := exchange(conn, r, "/seq/"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if handled.Load() != rounds {
		t.Errorf("the handler ran %d times, want %d", handled.Load(), rounds)
	}
	hits, misses := srv.stats.hits.Load(), srv.stats.misses.Load()
	t.Logf("%s", srv.StatsLine())
	if hits < rounds/4 {
		t.Errorf("only %d of %d requests were found by a predictive read (%d misses)", hits, rounds, misses)
	}
}

func TestPredictTwoAlternatingClientsOnSockets(t *testing.T) {
	twoPs(t)
	var handled atomic.Int64
	addr, srv := predictServer(t, sizeHandler(&handled))
	connA, rA := dial(t, addr)
	connB, rB := dial(t, addr)
	const rounds = 1000
	for i := 0; i < rounds; i++ {
		if err := exchange(connA, rA, "/a/"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
		if err := exchange(connB, rB, "/b/"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if handled.Load() != 2*rounds {
		t.Errorf("the handler ran %d times, want %d", handled.Load(), 2*rounds)
	}
	t.Logf("%s", srv.StatsLine())
	if hits := srv.stats.hits.Load(); hits < rounds/2 {
		t.Errorf("only %d of %d requests were found by a predictive read (%d misses)", hits, 2*rounds, srv.stats.misses.Load())
	}
}

// A second connection must not wait behind the predicted one for longer than the limits allow: one
// waiting read (3 ms) when the predicted connection has gone quiet, one window of predictive reads
// (2 ms) when it is busy. Scheduling noise is taken out by measuring several times and judging the
// best; every single measurement must still be far from a stall.
func TestPredictDoesNotStarveASecondConnection(t *testing.T) {
	twoPs(t)
	addr, srv := predictServer(t, sizeHandler(nil))
	connA, rA := dial(t, addr)
	connB, rB := dial(t, addr)
	if err := exchange(connB, rB, "/b/hello"); err != nil {
		t.Fatal(err)
	}
	seq := 0
	sequential := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			seq++
			if err := exchange(connA, rA, "/a/"+strconv.Itoa(seq)); err != nil {
				t.Fatal(err)
			}
		}
	}
	judge := func(what string, took []time.Duration) {
		t.Helper()
		best, worst := took[0], took[0]
		for _, d := range took {
			best, worst = min(best, d), max(worst, d)
		}
		t.Logf("%s: the second connection waited %v", what, took)
		if best > 5*time.Millisecond {
			t.Errorf("%s: the second connection was never answered within 5 ms (best %v)", what, best)
		}
		if worst > 250*time.Millisecond {
			t.Errorf("%s: the second connection waited %v", what, worst)
		}
	}

	// The predicted connection goes quiet: the loop is in a waiting read on it when the second
	// connection's request arrives. Each such miss raises the streak the loop asks for, so enough
	// exchanges go before each measurement to arm it again whatever that has become.
	var quiet []time.Duration
	for try := 0; try < 5; try++ {
		sequential(predictMost + 8)
		start := time.Now()
		if err := exchange(connB, rB, "/b/quiet/"+strconv.Itoa(try)); err != nil {
			t.Fatal(err)
		}
		quiet = append(quiet, time.Since(start))
	}
	judge("predicted connection quiet", quiet)
	if srv.stats.hits.Load() == 0 {
		t.Errorf("no predictive read ever found a request, so this measured nothing: %s", srv.StatsLine())
	}

	// The predicted connection stays busy, one request after the other, for the whole time.
	stop := make(chan struct{})
	busyDone := make(chan error, 1)
	connC, rC := dial(t, addr)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				busyDone <- nil
				return
			default:
			}
			if err := exchange(connC, rC, "/c/"+strconv.Itoa(i)); err != nil {
				busyDone <- err
				return
			}
		}
	}()
	var busy []time.Duration
	for try := 0; try < 8; try++ {
		time.Sleep(60 * time.Millisecond) // long enough for the busy connection to be predicted again
		start := time.Now()
		if err := exchange(connB, rB, "/b/busy/"+strconv.Itoa(try)); err != nil {
			t.Fatal(err)
		}
		busy = append(busy, time.Since(start))
	}
	close(stop)
	if err := <-busyDone; err != nil {
		t.Fatal(err)
	}
	judge("predicted connection busy", busy)
	t.Logf("%s", srv.StatsLine())
}

func TestPredictPipeliningOnSockets(t *testing.T) {
	twoPs(t)
	var handled atomic.Int64
	addr, srv := predictServer(t, sizeHandler(&handled))
	conn, r := dial(t, addr)
	for i := 0; i < 3*predictAfter; i++ {
		if err := exchange(conn, r, "/warm/"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Three requests in one segment, again and again: each batch is one read for the server, and
	// the three replies must come back in order.
	for i := 0; i < 300; i++ {
		tags := [3]string{"/p/" + strconv.Itoa(i) + "/x", "/p/" + strconv.Itoa(i) + "/y", "/p/" + strconv.Itoa(i) + "/z"}
		io.WriteString(conn, "GET "+tags[0]+get+"GET "+tags[1]+get+"GET "+tags[2]+get)
		for _, tag := range tags {
			if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != tag+"|" {
				t.Fatalf("pipelined %s: %d %q %v", tag, status, body, err)
			}
		}
	}
	// A request that arrives in two parts, the second after the waiting read has returned the first.
	for i := 0; i < 20; i++ {
		for j := 0; j < 3*predictAfter; j++ {
			if err := exchange(conn, r, "/between/"+strconv.Itoa(i)+"/"+strconv.Itoa(j)); err != nil {
				t.Fatal(err)
			}
		}
		body := strings.Repeat("b", 100+i)
		whole := "POST /split/" + strconv.Itoa(i) + " HTTP/1.1\r\nHost: h\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
		io.WriteString(conn, whole[:len(whole)-50])
		time.Sleep(time.Millisecond)
		io.WriteString(conn, whole[len(whole)-50:])
		if status, got, err := readResponse(r); err != nil || status != 200 || string(got) != "/split/"+strconv.Itoa(i)+"|"+body {
			t.Fatalf("split request %d: %d %q %v", i, status, got, err)
		}
	}
	want := int64(3*predictAfter + 900 + 20*(3*predictAfter+1))
	if handled.Load() != want {
		t.Errorf("the handler ran %d times, want %d", handled.Load(), want)
	}
	t.Logf("%s", srv.StatsLine())
}

// Half-closes with the loop in its waiting read: a client that sends its last request and closes
// its side gets the reply and then the server's close; a client that just closes its side gets the
// close. Nothing is left open.
func TestPredictHalfClose(t *testing.T) {
	twoPs(t)
	addr, srv := predictServer(t, sizeHandler(nil))
	warm := func(conn net.Conn, r *bufio.Reader) {
		t.Helper()
		for i := 0; i < 3*predictAfter; i++ {
			if err := exchange(conn, r, "/warm/"+strconv.Itoa(i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Once without counting, so the pool and the tables have their size.
	conn, r := dial(t, addr)
	warm(conn, r)
	conn.Close()
	time.Sleep(50 * time.Millisecond)
	before := openDescriptors(t)

	for i := 0; i < 20; i++ {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		tcp := conn.(*net.TCPConn)
		tcp.SetDeadline(time.Now().Add(20 * time.Second))
		r := bufio.NewReader(conn)
		warm(conn, r)
		if i%2 == 0 {
			io.WriteString(conn, "GET /last"+get)
			tcp.CloseWrite()
			if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != "/last|" {
				t.Fatalf("half-closed exchange %d: %d %q %v", i, status, body, err)
			}
		} else {
			tcp.CloseWrite()
		}
		if _, err := r.ReadByte(); err != io.EOF {
			t.Fatalf("connection %d: after a half-close the server should close, got %v", i, err)
		}
		conn.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for openDescriptors(t) > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := openDescriptors(t); after > before {
		t.Errorf("descriptors: %d before, %d after 20 connections", before, after)
	}
	t.Logf("%s", srv.StatsLine())
	if srv.stats.hits.Load() == 0 {
		t.Error("no predictive read ever found a request, so this tested nothing")
	}
}

// One P, clients in the same process: nobody can send while the loop holds the processor in a
// waiting read, so every such read runs into its limit. That must cost a few milliseconds a time
// and less and less often, never a stall or a lost request, and other goroutines must keep running.
func TestPredictWithOneP(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var handled atomic.Int64
	addr, srv := predictServer(t, sizeHandler(&handled))
	ticks := make(chan struct{}, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		ticks <- struct{}{}
	}()
	conn, r := dial(t, addr)
	start := time.Now()
	// With one P a client in this process is only woken by the runtime's monitor thread, some
	// milliseconds after its reply was sent, so the rounds are few: enough to arm the read at 34
	// exchanges and again, with the doubled requirement, at 98.
	const rounds = 120
	for i := 0; i < rounds; i++ {
		if err := exchange(conn, r, "/one/"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d exchanges in %v; %s", rounds, time.Since(start), srv.StatsLine())
	select {
	case <-ticks:
	case <-time.After(5 * time.Second):
		t.Error("a timer in another goroutine never fired while the loop was running")
	}
	if handled.Load() != rounds {
		t.Errorf("the handler ran %d times, want %d", handled.Load(), rounds)
	}
	if misses := srv.stats.misses.Load(); misses < 1 || misses > 3 {
		t.Errorf("%d predictive reads ran into their limit; with the back-off it should be 2", misses)
	}
	if hits := srv.stats.hits.Load(); hits != 0 {
		t.Logf("%d predictive reads found a request although the client could not run", hits)
	}
}

// The deferred accept path on its own: connections are read straight after being accepted, and a
// client that connects and stays silent for a while is still served when it finally speaks.
func TestEpollDeferAccept(t *testing.T) {
	var handled atomic.Int64
	srv := &Server{Handler: sizeHandler(&handled), IO: IOEpoll, DeferAccept: true, Count: true}
	addr := runEpoll(t, srv)
	for i := 0; i < 50; i++ {
		conn, r := dial(t, addr)
		for j := 0; j < 3; j++ {
			if err := exchange(conn, r, "/d/"+strconv.Itoa(i)+"/"+strconv.Itoa(j)); err != nil {
				t.Fatal(err)
			}
		}
		conn.Close()
	}
	quiet, r := dial(t, addr)
	time.Sleep(1500 * time.Millisecond)
	if err := exchange(quiet, r, "/late"); err != nil {
		t.Fatal(err)
	}
	if handled.Load() != 151 {
		t.Errorf("the handler ran %d times, want 151", handled.Load())
	}
	t.Logf("%s", srv.StatsLine())
	if accepted := srv.stats.accepted.Load(); accepted != 51 {
		t.Errorf("%d connections accepted, want 51", accepted)
	}
}
