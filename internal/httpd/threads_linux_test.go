//go:build linux

package httpd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The threads loop is tested like the epoll loop: every engine test of this package runs again
// with the server on it, and the tests below go after what only this loop has to get right
// (blocking reads and writes, the slots and the hand-over to the netpoll loop, connections that end
// while a goroutine waits on them, shutting down goroutines that sit in system calls).

// runThreads starts srv on the threads loop and returns its address. When the test ends the server
// is shut down, and Serve must return with every connection's goroutine gone.
func runThreads(t *testing.T, srv *Server) string {
	t.Helper()
	srv.IO = IOThreads
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
				t.Errorf("the threads loop ended with %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the threads loop did not stop after Shutdown")
		}
	})
	return addr
}

func threadsServer(t *testing.T, h Handler) (string, *Server) {
	t.Helper()
	srv := &Server{Handler: h, Token: "secret", DefaultHost: "fallback:1"}
	return runThreads(t, srv), srv
}

// slots waits until the loop holds want connections with a thread, and returns how many it has
// handed to the netpoll loop so far.
func slots(t *testing.T, srv *Server, want int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if th := srv.threads.Load(); th != nil {
			held, spilt := th.held()
			if held == want {
				return spilt
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d connections have a thread, want %d", held, want)
			}
		} else if time.Now().After(deadline) {
			t.Fatal("the threads loop did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestThreadsRunsTheEngineTests repeats the socket tests of httpd_test.go on the threads loop.
func TestThreadsRunsTheEngineTests(t *testing.T) {
	testIO = IOThreads
	defer func() { testIO = IONetpoll }()
	for name, test := range engineTests() {
		t.Run(name, test)
	}
}

// The same again with a goroutine that yields before every read, and with one that never does.
func TestThreadsRunsTheEngineTestsWithOtherYields(t *testing.T) {
	testIO = IOThreads
	defer func() { testIO, testTune = IONetpoll, nil }()
	for _, yield := range []time.Duration{time.Nanosecond, -1} {
		testTune = func(s *Server) { s.Yield = yield }
		for name, test := range engineTests() {
			t.Run(name+"/"+yield.String(), test)
		}
	}
}

// One client, one connection, one request after the other: the workload the loop is for.
func TestThreadsSequentialKeepAlive(t *testing.T) {
	for _, yield := range []time.Duration{0, time.Nanosecond, -1} {
		var handled atomic.Int64
		srv := &Server{Handler: sizeHandler(&handled), Yield: yield}
		conn, r := dial(t, runThreads(t, srv))
		const requests = 3000
		for i := 0; i < requests; i++ {
			if err := exchange(conn, r, "/seq/"+strconv.Itoa(i)); err != nil {
				t.Fatalf("yield %v, request %d: %v", yield, i, err)
			}
		}
		if handled.Load() != requests {
			t.Errorf("yield %v: the handler ran %d times, want %d", yield, handled.Load(), requests)
		}
		if spilt := slots(t, srv, 1); spilt != 0 {
			t.Errorf("yield %v: %d connections were handed to the netpoll loop", yield, spilt)
		}
		conn.Close()
		slots(t, srv, 0)
	}
}

// A request whose head and body arrive in several writes, with pauses in which the goroutine sits
// in its read: with a declared length, and chunked.
func TestThreadsBodySplitAcrossWrites(t *testing.T) {
	addr, _ := threadsServer(t, sizeHandler(nil))
	conn, r := dial(t, addr)
	payload := string(pattern(100000))
	pieces := []string{
		"POST /split HT",
		"TP/1.1\r\nHost: h\r\nContent-Le",
		"ngth: " + strconv.Itoa(len(payload)) + "\r\n\r",
		"\n" + payload[:10],
		payload[10:50000],
		payload[50000:],
	}
	for _, piece := range pieces {
		if _, err := io.WriteString(conn, piece); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != "/split|"+payload {
		t.Fatalf("split request: %d, %d bytes, %v", status, len(body), err)
	}
	chunks := []string{
		"POST /chunked HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhe",
		"llo\r\n",
		"6\r\n world\r\n0\r",
		"\n\r\n",
	}
	for _, piece := range chunks {
		if _, err := io.WriteString(conn, piece); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != "/chunked|hello world" {
		t.Fatalf("chunked request in pieces: %d %q %v", status, body, err)
	}
	if err := exchange(conn, r, "/after"); err != nil {
		t.Fatalf("request after the split ones: %v", err)
	}
}

// Several requests in one write are answered in order, and so is one that is only complete with
// the next write, with a request of its own right behind it.
func TestThreadsPipelinedRequests(t *testing.T) {
	var handled atomic.Int64
	addr, _ := threadsServer(t, sizeHandler(&handled))
	conn, r := dial(t, addr)
	for round := 0; round < 50; round++ {
		tag := "/p" + strconv.Itoa(round)
		batch := ""
		for i := 0; i < 8; i++ {
			batch += "GET " + tag + "/" + strconv.Itoa(i) + get
		}
		last := "POST " + tag + "/last HTTP/1.1\r\nHost: h\r\nContent-Length: 6\r\n\r\nabcdef"
		if _, err := io.WriteString(conn, batch+last[:len(last)-4]); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 8; i++ {
			want := tag + "/" + strconv.Itoa(i) + "|"
			if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != want {
				t.Fatalf("pipelined %s: %d %q %v", want, status, body, err)
			}
		}
		if _, err := io.WriteString(conn, last[len(last)-4:]+"GET "+tag+"/tail"+get); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{tag + "/last|abcdef", tag + "/tail|"} {
			if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != want {
				t.Fatalf("pipelined %s: %d %q %v", want, status, body, err)
			}
		}
	}
	if handled.Load() != 50*10 {
		t.Errorf("the handler ran %d times, want %d", handled.Load(), 50*10)
	}
}

// A response far larger than the socket buffer, to a client that is not reading: the connection's
// goroutine waits in its write, everyone else is served meanwhile, and every byte arrives in order
// once the client reads. Requests pipelined behind the large one are answered after it.
func TestThreadsLargeResponse(t *testing.T) {
	addr, srv := threadsServer(t, sizeHandler(nil))
	const big = 24 << 20
	slow, slowReader := dial(t, addr)
	slow.SetReadBuffer(64 << 10) // not below one loopback segment; see TestEpollPartialWrites
	if _, err := io.WriteString(slow, "GET /bytes/"+strconv.Itoa(big)+get+"GET /after-1"+get+"GET /bytes/70000"+get+"GET /after-2"+get); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the server is now stuck mid-response on this connection

	for i := 0; i < 20; i++ {
		other, r := dial(t, addr)
		if err := exchange(other, r, "/other/"+strconv.Itoa(i)); err != nil {
			t.Fatalf("connection %d while another is blocked: %v", i, err)
		}
		other.Close()
	}

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
	io.WriteString(slow, "POST /again HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\n\r\nhello")
	if status, body, err := readResponse(slowReader); err != nil || status != 200 || string(body) != "/again|hello" {
		t.Fatalf("request after the large response: %d %q %v", status, body, err)
	}
	slots(t, srv, 1)
}

// More connections than slots, all open at once: the first threadsMax have a thread, the rest are
// served by the netpoll loop, nobody waits and every answer is the right one. A slot is free again
// as soon as its connection closes.
func TestThreadsMoreConnectionsThanSlots(t *testing.T) {
	var handled atomic.Int64
	addr, srv := threadsServer(t, sizeHandler(&handled))
	before := openDescriptors(t)
	const connections, rounds = threadsMax + 72, 8
	conns := make([]*net.TCPConn, connections)
	readers := make([]*bufio.Reader, connections)
	for i := range conns {
		conns[i], readers[i] = dial(t, addr)
	}
	// All of them are open; they talk at the same time, and each one's answers must be its own.
	round := func(n int) {
		t.Helper()
		var wg sync.WaitGroup
		failures := make(chan error, connections)
		for i := range conns {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				tag := fmt.Sprintf("/c%d/r%d", i, n)
				var err error
				switch n % 3 {
				case 0:
					err = exchange(conns[i], readers[i], tag)
				case 1: // a body in two pieces
					payload := strings.Repeat(strconv.Itoa(i%10), 2000+i)
					io.WriteString(conns[i], "POST "+tag+" HTTP/1.1\r\nHost: h\r\nContent-Length: "+strconv.Itoa(len(payload))+"\r\n\r\n"+payload[:100])
					time.Sleep(time.Millisecond)
					io.WriteString(conns[i], payload[100:])
					status, body, rerr := readResponse(readers[i])
					if rerr != nil || status != 200 || string(body) != tag+"|"+payload {
						err = fmt.Errorf("%s: %d, %d bytes, %v", tag, status, len(body), rerr)
					}
				case 2: // a response of a few hundred kilobytes
					size := 300000 + i*41
					io.WriteString(conns[i], "GET /bytes/"+strconv.Itoa(size)+get)
					status, body, rerr := readResponse(readers[i])
					if rerr != nil || status != 200 || len(body) != size || string(body[:64]) != string(pattern(size)[:64]) {
						err = fmt.Errorf("%s: %d, %d bytes, %v", tag, status, len(body), rerr)
					}
				}
				if err != nil {
					failures <- err
				}
			}(i)
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
	}
	for n := 0; n < rounds; n++ {
		round(n)
		if t.Failed() {
			t.FailNow()
		}
	}
	if spilt := slots(t, srv, threadsMax); spilt != connections-threadsMax {
		t.Errorf("%d connections were handed to the netpoll loop, want %d", spilt, connections-threadsMax)
	}
	if handled.Load() != connections*rounds {
		t.Errorf("the handler ran %d times, want %d", handled.Load(), connections*rounds)
	}

	// Close half of them: their slots come back, and new connections get threads again.
	for i := 0; i < connections; i += 2 {
		conns[i].Close()
	}
	// Connections are accepted in the order they were made, so the first threadsMax have the
	// threads and every second one of those is gone now.
	slots(t, srv, threadsMax/2)
	held, spilt := srv.threads.Load().held()
	fresh, r := dial(t, addr)
	if err := exchange(fresh, r, "/fresh"); err != nil {
		t.Fatalf("a connection after others closed: %v", err)
	}
	if nowHeld, nowSpilt := srv.threads.Load().held(); held >= threadsMax || nowHeld != held+1 || nowSpilt != spilt {
		t.Errorf("a new connection with free slots: %d with a thread before, %d after; %d handed over before, %d after", held, nowHeld, spilt, nowSpilt)
	}
	// The survivors still work, then everything closes and nothing is left behind.
	for i := 1; i < connections; i += 2 {
		if err := exchange(conns[i], readers[i], "/still/"+strconv.Itoa(i)); err != nil {
			t.Fatalf("connection %d after others closed: %v", i, err)
		}
		conns[i].Close()
	}
	fresh.Close()
	slots(t, srv, 0)
	deadline := time.Now().Add(5 * time.Second)
	for openDescriptors(t) > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := openDescriptors(t); after > before {
		t.Errorf("%d descriptors open after all connections closed, %d before", after, before)
	}
}

// Connections that end in every possible way while a goroutine waits on them: closed between
// requests, closed in the middle of a head or a body, half-closed after a request and in the
// middle of one, reset while the server writes. What was complete is answered, every slot and
// descriptor is released, and the server keeps serving.
func TestThreadsClientClosesMidRequest(t *testing.T) {
	var handled atomic.Int64
	addr, srv := threadsServer(t, sizeHandler(&handled))
	before := openDescriptors(t)
	for i := 0; i < 30; i++ {
		// Closed between requests.
		conn, r := dial(t, addr)
		if err := exchange(conn, r, "/a/"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
		conn.Close()

		// Closed in the middle of the head, and in the middle of the body.
		conn, _ = dial(t, addr)
		io.WriteString(conn, "GET /never HTTP/1.1\r\nHost:")
		conn.Close()
		conn, _ = dial(t, addr)
		io.WriteString(conn, "POST /never HTTP/1.1\r\nHost: h\r\nContent-Length: 1000\r\n\r\nonly this much")
		time.Sleep(time.Millisecond) // the goroutine is in its read for the rest when the close comes
		conn.Close()

		// Half-closed after a complete request: the answer comes, then the server closes too.
		conn, r = dial(t, addr)
		io.WriteString(conn, "GET /half/"+strconv.Itoa(i)+get)
		conn.CloseWrite()
		if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != "/half/"+strconv.Itoa(i)+"|" {
			t.Fatalf("half-closed connection: %d %q %v", status, body, err)
		}
		if _, err := r.ReadByte(); err != io.EOF {
			t.Fatalf("after a half-close the server should close, got %v", err)
		}
		conn.Close()

		// Half-closed in the middle of a request: nothing to answer, the server closes.
		conn, r = dial(t, addr)
		io.WriteString(conn, "POST /never HTTP/1.1\r\nHost: h\r\nContent-Length: 50\r\n\r\npart")
		conn.CloseWrite()
		if _, err := r.ReadByte(); err != io.EOF {
			t.Fatalf("after a half-close in mid-request the server should close, got %v", err)
		}
		conn.Close()

		// Reset while the server is writing a large response to it.
		conn, _ = dial(t, addr)
		conn.SetReadBuffer(64 << 10)
		io.WriteString(conn, "GET /bytes/"+strconv.Itoa(8<<20)+get)
		time.Sleep(2 * time.Millisecond)
		conn.SetLinger(0)
		conn.Close()
	}
	conn, r := dial(t, addr)
	if err := exchange(conn, r, "/alive"); err != nil {
		t.Fatalf("the server after all those endings: %v", err)
	}
	conn.Close()
	slots(t, srv, 0)
	// The complete requests ran once each: two per round that always arrive, the large one unless
	// its connection was reset before the request was read, and the last one.
	if n := handled.Load(); n < 30*2+1 || n > 30*3+1 {
		t.Errorf("the handler ran %d times, want %d to %d", n, 30*2+1, 30*3+1)
	}
	deadline := time.Now().Add(5 * time.Second)
	for openDescriptors(t) > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := openDescriptors(t); after > before {
		t.Errorf("%d descriptors open after all connections ended, %d before", after, before)
	}
}

// Shutdown ends the loop while goroutines sit in reads and in a write to a client that does not
// read: every one of them comes out of its system call and closes its connection, and (checked by
// runThreads) Serve returns.
func TestThreadsShutdownWithOpenConnections(t *testing.T) {
	addr, srv := threadsServer(t, sizeHandler(nil))
	var idle []*bufio.Reader
	for i := 0; i < 10; i++ {
		conn, r := dial(t, addr)
		if err := exchange(conn, r, "/idle/"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
		idle = append(idle, r)
	}
	stuck, _ := dial(t, addr)
	stuck.SetReadBuffer(64 << 10)
	io.WriteString(stuck, "GET /bytes/"+strconv.Itoa(16<<20)+get)
	time.Sleep(100 * time.Millisecond)
	slots(t, srv, 11)
	// What Shutdown does, without the listener that runThreads keeps to itself.
	srv.stopping.Store(true)
	srv.threads.Load().halt()
	for i, r := range idle {
		if _, err := r.ReadByte(); err != io.EOF {
			t.Errorf("idle connection %d after Shutdown: %v, want EOF", i, err)
		}
	}
	slots(t, srv, 0)
}

// The loop with a single P: every blocking read gives the P away, which is slow but must work.
func TestThreadsWithOneP(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	addr, _ := threadsServer(t, sizeHandler(nil))
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
			conn.SetDeadline(time.Now().Add(30 * time.Second))
			r := bufio.NewReader(conn)
			for i := 0; i < 50; i++ {
				if err := exchange(conn, r, fmt.Sprintf("/one-p/%d/%d", id, i)); err != nil {
					failures <- err
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
}
