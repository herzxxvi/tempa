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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The hybrid loop is tested like the other loops: every engine test of this package runs again
// with the server on it, and the tests below go after what only this loop has to get right
// (a connection changing hands between the loop and a goroutine, in both directions, with
// requests on their way; connections ending while they are out; shutting down with connections in
// every place a connection can be). None of them depends on how fast anything is: they wait for
// what they need to see, with generous limits.

// runHybrid starts srv on the hybrid loop and returns its address. When the test ends the server
// is shut down, and Serve must return.
func runHybrid(t *testing.T, srv *Server) string {
	t.Helper()
	srv.IO = IOHybrid
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
				t.Errorf("the hybrid loop ended with %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the hybrid loop did not stop after Shutdown")
		}
	})
	return addr
}

func hybridServer(t *testing.T, h Handler) (string, *Server) {
	t.Helper()
	srv := &Server{Handler: h, Token: "secret", DefaultHost: "fallback:1", Count: true}
	return runHybrid(t, srv), srv
}

// lending is the hybrid loop's state: connections out now, lent and given back so far.
func lending(t *testing.T, srv *Server) (out int, lent, given int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if hy := srv.hybrid.Load(); hy != nil {
			return hy.held()
		}
		if time.Now().After(deadline) {
			t.Fatal("the hybrid loop did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

// waitOut waits until want connections are out.
func waitOut(t *testing.T, srv *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, _, _ := lending(t, srv)
		if out == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections are out, want %d", out, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// lend runs exchanges on conn, one after the other, until the loop has lent a connection once
// more, and returns how many it took. Nothing else may be talking to the server meanwhile. *seq
// numbers the requests.
func lend(t *testing.T, srv *Server, conn net.Conn, r *bufio.Reader, seq *int) int {
	t.Helper()
	_, before, _ := lending(t, srv)
	for i := 1; i <= 20000; i++ {
		*seq++
		if err := exchange(conn, r, "/lend/"+strconv.Itoa(*seq)); err != nil {
			t.Fatal(err)
		}
		if _, lent, _ := lending(t, srv); lent > before {
			return i
		}
	}
	t.Fatal("the connection was never lent out")
	return 0
}

// TestHybridRunsTheEngineTests repeats the socket tests of httpd_test.go on the hybrid loop: as it
// is, where their few requests per connection all stay in the loop, and with a connection lent
// out after a streak of one, so that requests in pieces, chunked bodies, pipelines and closes meet
// a connection that is out or changing hands.
func TestHybridRunsTheEngineTests(t *testing.T) {
	testIO = IOHybrid
	defer func() { testIO, testTune = IONetpoll, nil }()
	for name, test := range engineTests() {
		t.Run(name, test)
	}
	testTune = func(srv *Server) { srv.hybridAfter = 1 }
	for name, test := range engineTests() {
		t.Run(name+"/eager", test)
	}
}

// One client, one connection, one request after the other: the connection is lent out, not before
// the streak asked for, and served to the end wherever it is.
func TestHybridLendsASequentialConnection(t *testing.T) {
	var handled atomic.Int64
	addr, srv := hybridServer(t, sizeHandler(&handled))
	conn, r := dial(t, addr)
	seq := 0
	for ; seq < hybridAfter; seq++ {
		if err := exchange(conn, r, "/early/"+strconv.Itoa(seq)); err != nil {
			t.Fatal(err)
		}
	}
	if out, lent, _ := lending(t, srv); out != 0 || lent != 0 {
		t.Fatalf("after %d exchanges %d connections are out (%d lent so far), want none", hybridAfter, out, lent)
	}
	lend(t, srv, conn, r, &seq)
	for i := 0; i < 2000; i++ {
		seq++
		if err := exchange(conn, r, "/out/"+strconv.Itoa(seq)); err != nil {
			t.Fatal(err)
		}
	}
	if int(handled.Load()) != seq {
		t.Errorf("the handler ran %d times, want %d", handled.Load(), seq)
	}
	conn.Close()
	waitOut(t, srv, 0)
}

// A lent connection that goes quiet is given back, served by the loop when it speaks again, and
// lent out again when it keeps speaking.
func TestHybridGivesBackAndLendsAgain(t *testing.T) {
	var handled atomic.Int64
	addr, srv := hybridServer(t, sizeHandler(&handled))
	conn, r := dial(t, addr)
	seq := 0
	for round := 0; round < 3; round++ {
		lend(t, srv, conn, r, &seq)
		// A stay long enough to count as worth it, unless the machine stalls: then the next round
		// only needs more exchanges.
		for i := 0; i < 3*hybridAfter; i++ {
			seq++
			if err := exchange(conn, r, "/stay/"+strconv.Itoa(seq)); err != nil {
				t.Fatal(err)
			}
		}
		_, _, before := lending(t, srv)
		waitOut(t, srv, 0) // silence: the read gives up
		if _, _, given := lending(t, srv); given < before || given == 0 {
			t.Fatalf("round %d: %d connections given back so far, %d before the pause", round, given, before)
		}
		// The first request after the pause is the loop's.
		seq++
		if err := exchange(conn, r, "/back/"+strconv.Itoa(seq)); err != nil {
			t.Fatalf("round %d, first request after the pause: %v", round, err)
		}
	}
	if int(handled.Load()) != seq {
		t.Errorf("the handler ran %d times, want %d", handled.Load(), seq)
	}
}

// While one connection is out, and even while its request is being handled, the loop serves every
// other connection: nobody waits for anybody else.
func TestHybridServesOthersWhileOneIsOut(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	echo := sizeHandler(nil)
	addr, srv := hybridServer(t, func(c *Ctx) {
		if string(c.Path) == "/slow" {
			entered <- struct{}{}
			<-release
		}
		echo(c)
	})
	a, ar := dial(t, addr)
	seq := 0
	for attempt := 0; ; attempt++ {
		if attempt == 20 {
			t.Fatal("the connection was never out while its slow request ran")
		}
		if out, _, _ := lending(t, srv); out == 0 {
			lend(t, srv, a, ar, &seq)
		}
		if _, err := io.WriteString(a, "GET /slow"+get); err != nil {
			t.Fatal(err)
		}
		<-entered
		// The handler is running. If the connection is out now, it stays out until release.
		if out, _, _ := lending(t, srv); out == 1 {
			break
		}
		// It was given back just before the request came, and the handler holds the loop.
		release <- struct{}{}
		if status, _, err := readResponse(ar); err != nil || status != 200 {
			t.Fatalf("slow request in the loop: %d %v", status, err)
		}
	}
	// Other connections, new and used, pipelined and not, while the slow request is held.
	for id := 0; id < 3; id++ {
		b, br := dial(t, addr)
		for i := 0; i < 30; i++ {
			if err := exchange(b, br, fmt.Sprintf("/other/%d/%d", id, i)); err != nil {
				t.Fatalf("another connection while one is out: %v", err)
			}
		}
		if _, err := io.WriteString(b, "GET /p1"+get+"GET /p2"+get); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"/p1|", "/p2|"} {
			if status, body, err := readResponse(br); err != nil || status != 200 || string(body) != want {
				t.Fatalf("pipelined on another connection: %d %q %v, want %q", status, body, err, want)
			}
		}
	}
	// The others may have been lent out too by now; the one with the slow request still is.
	if out, _, _ := lending(t, srv); out < 1 {
		t.Errorf("%d connections are out while the slow request is held", out)
	}
	release <- struct{}{}
	if status, body, err := readResponse(ar); err != nil || status != 200 || string(body) != "/slow|" {
		t.Fatalf("the slow request: %d %q %v", status, body, err)
	}
	if err := exchange(a, ar, "/after"); err != nil {
		t.Fatal(err)
	}
}

// Requests on their way while the connection changes hands, in both directions: pipelined
// batches before, across and after the loop lends the connection, and a request and a body that
// are cut in two by the pause in which it is given back. Every reply comes, once, in order.
func TestHybridRequestsAcrossTheSwitch(t *testing.T) {
	var handled atomic.Int64
	addr, srv := hybridServer(t, sizeHandler(&handled))
	conn, r := dial(t, addr)
	sent := 0
	batch := func(size int) {
		t.Helper()
		var raw []byte
		for i := 0; i < size; i++ {
			raw = append(raw, "GET /pipe/"+strconv.Itoa(sent+i)+get...)
		}
		if _, err := conn.Write(raw); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < size; i++ {
			want := "/pipe/" + strconv.Itoa(sent+i) + "|"
			if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != want {
				t.Fatalf("pipelined request %d: %d %q %v, want %q", sent+i, status, body, err, want)
			}
		}
		sent += size
	}
	// untilLent sends batches until the loop has lent the connection once more.
	untilLent := func() {
		t.Helper()
		_, before, _ := lending(t, srv)
		for i := 0; ; i++ {
			if i == 20000 {
				t.Fatal("the pipelining connection was never lent out")
			}
			batch(1 + i%4)
			if _, lent, _ := lending(t, srv); lent > before {
				return
			}
		}
	}
	payload := string(pattern(50000))
	for round := 0; round < 3; round++ {
		untilLent()
		for i := 0; i < 50; i++ {
			batch(1 + i%5)
		}
		// A request cut in two by a pause: the first half is read wherever the connection is, the
		// connection is given back with it (or already was), and the loop reads the rest.
		head := "POST /cut/" + strconv.Itoa(round) + " HTTP/1.1\r\nHost: h\r\nContent-Length: " + strconv.Itoa(len(payload)) + "\r\n\r\n"
		cut := []int{7, len(head) - 2, len(head) + 20000}[round]
		whole := head + payload
		if _, err := io.WriteString(conn, whole[:cut]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * hybridIdleMillis * time.Millisecond)
		waitOut(t, srv, 0)
		if _, err := io.WriteString(conn, whole[cut:]); err != nil {
			t.Fatal(err)
		}
		want := "/cut/" + strconv.Itoa(round) + "|" + payload
		if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != want {
			t.Fatalf("round %d: the request cut at %d: %d, %d bytes, %v", round, cut, status, len(body), err)
		}
		batch(3)
	}
	// A large reply and a pipeline behind it, to a connection that is out.
	untilLent()
	if _, err := io.WriteString(conn, "GET /bytes/"+strconv.Itoa(4<<20)+get+"GET /behind"+get); err != nil {
		t.Fatal(err)
	}
	if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != string(pattern(4<<20)) {
		t.Fatalf("large reply: %d, %d bytes, %v", status, len(body), err)
	}
	if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != "/behind|" {
		t.Fatalf("behind the large reply: %d %q %v", status, body, err)
	}
	if want := int64(sent + 3 + 2); handled.Load() != want {
		t.Errorf("the handler ran %d times, want %d", handled.Load(), want)
	}
}

// Clients that go away while their connection is out: closed between requests, closed and
// half-closed in the middle of one, half-closed after one, reset during a large reply. What was
// complete is answered, every descriptor is released, and the server keeps serving.
func TestHybridClientClosesWhileOut(t *testing.T) {
	addr, srv := hybridServer(t, sizeHandler(nil))
	// One exchange first, so that everything the server opens lazily is open before counting.
	first, fr := dial(t, addr)
	if err := exchange(first, fr, "/first"); err != nil {
		t.Fatal(err)
	}
	first.Close()
	time.Sleep(50 * time.Millisecond)
	before := openDescriptors(t)
	seq := 0
	for round := 0; round < 5; round++ {
		conn, r := dial(t, addr)
		lend(t, srv, conn, r, &seq)
		switch round {
		case 0: // closed between requests
		case 1: // closed in the middle of a request
			io.WriteString(conn, "POST /never HTTP/1.1\r\nHost: h\r\nContent-Length: 1000\r\n\r\nonly this much")
			time.Sleep(time.Millisecond)
		case 2: // half-closed after a complete request: the answer comes, then the server closes
			io.WriteString(conn, "GET /half"+get)
			conn.CloseWrite()
			if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != "/half|" {
				t.Fatalf("half-closed connection: %d %q %v", status, body, err)
			}
			if _, err := r.ReadByte(); err != io.EOF {
				t.Fatalf("after a half-close the server should close, got %v", err)
			}
		case 3: // half-closed in the middle of a request: nothing to answer, the server closes
			io.WriteString(conn, "POST /never HTTP/1.1\r\nHost: h\r\nContent-Length: 50\r\n\r\npart")
			conn.CloseWrite()
			if _, err := r.ReadByte(); err != io.EOF {
				t.Fatalf("after a half-close in mid-request the server should close, got %v", err)
			}
		case 4: // reset while the server is writing a large reply to it
			conn.SetReadBuffer(64 << 10)
			io.WriteString(conn, "GET /bytes/"+strconv.Itoa(8<<20)+get)
			time.Sleep(2 * time.Millisecond)
			conn.SetLinger(0)
		}
		conn.Close()
		waitOut(t, srv, 0)
		// The connection may have been given back just before it ended; then the loop closes it,
		// which shows as the server still answering below and in the descriptor count at the end.
		other, or := dial(t, addr)
		if err := exchange(other, or, "/alive/"+strconv.Itoa(round)); err != nil {
			t.Fatalf("the server after ending %d: %v", round, err)
		}
		other.Close()
	}
	deadline := time.Now().Add(10 * time.Second)
	for openDescriptors(t) > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := openDescriptors(t); after > before {
		t.Errorf("%d descriptors open after all connections ended, %d before", after, before)
	}
}

// Shutdown with connections in the places one can be: idle in the loop, out and stuck in a write
// to a client that does not read, given back and not yet heard of again. Serve returns, every
// client sees its connection end, and no descriptor is left. (Out and busy: the next test.)
func TestHybridShutdownWithConnectionsOut(t *testing.T) {
	// What the runtime opens for its own poller is open before the count.
	if warm, err := Listen(); err == nil {
		warm.Close()
	}
	before := openDescriptors(t)
	srv := &Server{Handler: sizeHandler(nil), IO: IOHybrid}
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Serve(ln) }()

	var conns []*net.TCPConn
	var readers []*bufio.Reader
	open := func() (*net.TCPConn, *bufio.Reader) {
		conn, r := dial(t, addr)
		conns, readers = append(conns, conn), append(readers, r)
		return conn, r
	}
	seq := 0
	// Idle in the loop.
	for i := 0; i < 5; i++ {
		conn, r := open()
		if err := exchange(conn, r, "/idle/"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Given back and silent since.
	conn, r := open()
	lend(t, srv, conn, r, &seq)
	waitOut(t, srv, 0)
	// Out, and stuck in a write (unless it was given back in between: then the loop holds the
	// rest of the reply, which is a place to be shut down in as well).
	stuck, sr := open()
	lend(t, srv, stuck, sr, &seq)
	stuck.SetReadBuffer(64 << 10)
	io.WriteString(stuck, "GET /bytes/"+strconv.Itoa(16<<20)+get)
	time.Sleep(50 * time.Millisecond)

	srv.Shutdown(ln)
	select {
	case err := <-stopped:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("the hybrid loop ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hybrid loop did not stop after Shutdown")
	}
	if out, _, _ := lending(t, srv); out != 0 {
		t.Errorf("%d connections are still out after Shutdown", out)
	}
	for i, r := range readers {
		// Whatever was on its way is read off; then the connection has ended.
		if _, err := io.Copy(io.Discard, r); err != nil && !isReset(err) {
			t.Errorf("connection %d after Shutdown: %v, want its end", i, err)
		}
	}
	for _, conn := range conns {
		conn.Close()
	}
	deadline := time.Now().Add(10 * time.Second)
	for openDescriptors(t) > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := openDescriptors(t); after > before {
		t.Errorf("%d descriptors open after Shutdown, %d before the server started", after, before)
	}
}

// isReset reports whether err is what a client sees when the server closed a connection with
// unread input or unsent output on it.
func isReset(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && !op.Timeout()
}

// Shutdown while a connection is out and busy: its goroutine comes out of whatever call it is in.
func TestHybridShutdownWhileBusy(t *testing.T) {
	srv := &Server{Handler: sizeHandler(nil), IO: IOHybrid}
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Serve(ln) }()
	conn, r := dial(t, addr)
	seq := 0
	lend(t, srv, conn, r, &seq)
	done := make(chan int)
	go func() {
		n := 0
		for exchange(conn, r, "/busy/"+strconv.Itoa(n)) == nil {
			n++
		}
		done <- n
	}()
	time.Sleep(20 * time.Millisecond)
	srv.Shutdown(ln)
	select {
	case err := <-stopped:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("the hybrid loop ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hybrid loop did not stop after Shutdown")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the client of the lent connection never saw it end")
	}
	if out, _, _ := lending(t, srv); out != 0 {
		t.Errorf("%d connections are still out after Shutdown", out)
	}
}

// No more than hybridMax connections are out at a time. Each of them is kept out by a request whose
// handler does not return; the next connection that talks alone stays in the loop, and is served.
func TestHybridLendsOnlySoMany(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	echo := sizeHandler(nil)
	addr, srv := hybridServer(t, func(c *Ctx) {
		if string(c.Path) == "/hold" {
			entered <- struct{}{}
			<-release
		}
		echo(c)
	})
	seq := 0
	var held []*bufio.Reader
	for len(held) < hybridMax {
		conn, r := dial(t, addr)
		for attempt := 0; ; attempt++ {
			if attempt == 20 {
				t.Fatalf("connection %d was never out while its request was held", len(held))
			}
			lend(t, srv, conn, r, &seq)
			if _, err := io.WriteString(conn, "GET /hold"+get); err != nil {
				t.Fatal(err)
			}
			<-entered
			// The handler is running. If the connection is out now, it stays out until release.
			if out, _, _ := lending(t, srv); out == len(held)+1 {
				break
			}
			// It was given back just before the request came, and the handler holds the loop.
			release <- struct{}{}
			if status, _, err := readResponse(r); err != nil || status != 200 {
				t.Fatalf("held request in the loop: %d %v", status, err)
			}
		}
		held = append(held, r)
	}
	// Every place is taken.
	conn, r := dial(t, addr)
	_, before, _ := lending(t, srv)
	for i := 0; i < 20*hybridAfter; i++ {
		if err := exchange(conn, r, "/loop/"+strconv.Itoa(i)); err != nil {
			t.Fatalf("a connection beyond the limit: %v", err)
		}
	}
	if out, lent, _ := lending(t, srv); out != hybridMax || lent != before {
		t.Errorf("%d connections are out (%d lent since the limit was reached), want %d and none", out, lent-before, hybridMax)
	}
	for range held {
		release <- struct{}{}
	}
	for i, r := range held {
		if status, body, err := readResponse(r); err != nil || status != 200 || string(body) != "/hold|" {
			t.Errorf("held request %d: %d %q %v", i, status, body, err)
		}
	}
}

// Many clients at once, each sequential on its own connection, with pauses that let lent
// connections go back: connections change hands in both directions under load, and every reply is
// the right one.
func TestHybridManyClientsWithPauses(t *testing.T) {
	var handled atomic.Int64
	addr, _ := hybridServer(t, sizeHandler(&handled))
	const clients, rounds, perRound = 6, 4, 60
	var wg sync.WaitGroup
	failures := make(chan error, clients)
	for id := 0; id < clients; id++ {
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
				for i := 0; i < perRound; i++ {
					if err := exchange(conn, r, fmt.Sprintf("/many/%d/%d/%d", id, round, i)); err != nil {
						failures <- err
						return
					}
				}
				time.Sleep(time.Duration(id*15) * time.Millisecond)
			}
		}(id)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if want := int64(clients * rounds * perRound); handled.Load() != want {
		t.Errorf("the handler ran %d times, want %d", handled.Load(), want)
	}
}

// The loop with a single P: slow, since every blocking call gives the P away, but it must work,
// with a connection out and others in the loop.
func TestHybridWithOneP(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	addr, srv := hybridServer(t, sizeHandler(nil))
	conn, r := dial(t, addr)
	seq := 0
	lend(t, srv, conn, r, &seq)
	other, or := dial(t, addr)
	for i := 0; i < 40; i++ {
		if err := exchange(conn, r, "/one-p/a/"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
		if err := exchange(other, or, "/one-p/b/"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
}
