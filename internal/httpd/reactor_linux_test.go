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
	for name, test := range map[string]func(*testing.T){
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
	} {
		t.Run(name, test)
	}
}

// epollServer starts a server on the epoll loop and returns its address.
func epollServer(t *testing.T, h Handler) string {
	t.Helper()
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	srv := &Server{Handler: h, Token: "secret", DefaultHost: "fallback:1", IO: IOEpoll}
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
