//go:build linux

package httpd

import (
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// The threads loop: every connection has a goroutine that waits for its requests in a blocking
// read on a blocking socket, and answers with one write. An exchange on a persistent connection is
// two system calls, a read and a write: there is no wait that says which connection is ready and no
// read that only learns there is nothing more. Requests are handled by the same code as in the
// other loops (Server.answer, Ctx.settle), so what goes out on the wire is the same.
//
// The price is a thread: a goroutine in a blocking system call keeps its thread for as long as the
// call lasts. So only threadsMax connections are served this way at a time. A connection accepted
// while that many are open is handed to the netpoll loop at once (Server.ServeConn on a duplicate
// of its descriptor that the runtime's poller watches) and stays there for its lifetime; nothing
// ever waits for a slot.
//
// How it lives with the Go runtime. The calls are made with syscall.Syscall, which tells the
// scheduler that the goroutine is in a system call. What the scheduler does with that decides
// whether the two calls stay two:
//
//   - A goroutine entering a system call keeps its P, and takes it back on return without any
//     system call, unless the monitor thread took the P away meanwhile. Without its P it takes any
//     idle one, again for free; only when there is none is it queued and its thread put to sleep.
//   - The monitor leaves the P of a goroutine in a system call alone for 10 ms while other Ps are
//     idle. With no idle P it takes it at its second look (and starts a thread to use it): that is
//     what GOMAXPROCS=1 means for this loop, a futex call or two per request. The loop is meant to
//     run with GOMAXPROCS above the number of connections that are busy at the same time.
//   - The 10 ms hold only while the P's scheduling counter moves. A goroutine that goes from system
//     call to system call never passes through the scheduler, so after 10 ms the monitor counts it
//     as one that has run too long, and from then on takes its P at every look that finds it in a
//     call, each time falling back to its shortest sleep (20 µs): a steady stream of sleeps on the
//     monitor's thread, far more system calls than the loop saves. Hence Server.Yield: before
//     waiting for a request, a goroutine that has not passed through the scheduler for 5 ms does so
//     (runtime.Gosched). With nothing else to run it is back at once, and the cost is the wake-up
//     of one idle thread, at most once per 5 ms and connection instead of once per exchange.
//   - A read that waits longer than 10 ms loses its P whatever is done (and the monitor then sleeps
//     short for a while). That is the cost of a pause, not of an exchange.
//
// The accepting goroutine waits in a blocking accept4 in the same way. After starting a
// connection's goroutine it yields, so that the new goroutine runs on this thread at once and the
// accepting one moves to an idle P, instead of going back into accept4 with the new goroutine
// queued behind it on a P that is in a system call.

// ThreadsSupported reports whether this build has the threads loop.
const ThreadsSupported = true

const (
	// threadsMax is the most connections that have a thread of their own at one time. With the
	// rest of the process (and a launcher beside it) that stays far below a limit of 256 threads.
	threadsMax = 128
	// threadsYield is Server.Yield's default: well inside the monitor's 10 ms.
	threadsYield = 5 * time.Millisecond
)

// threads is the state of the threads loop.
type threads struct {
	srv   *Server
	lfd   int           // the listening socket (a duplicate of the listener's descriptor), blocking
	lfile *os.File      // owns lfd
	yield time.Duration // 0: never
	live  atomic.Int64  // open connections of both kinds (kept only with stats)

	mu      sync.Mutex
	fds     map[int]struct{} // the connections that have a thread, by descriptor
	halted  bool             // Shutdown was called: no connection is adopted any more
	closed  bool             // the loop has ended and lfd is no longer ours
	spilled int              // connections handed to the netpoll loop
	serving sync.WaitGroup   // the goroutines of the connections in fds
}

// serveThreads takes over the listener's socket and runs the accept loop. If the socket cannot be
// set up, it serves with the netpoll loop instead.
func (s *Server) serveThreads(ln *net.TCPListener) error {
	file, err := ln.File() // a duplicate descriptor; the socket stays open when ln is closed
	if err != nil {
		return s.serveNetpoll(ln)
	}
	lfd := int(file.Fd())
	// Blocking mode, which accepted sockets do not inherit but have by default. The mode is shared
	// with ln's descriptor, which is closed below.
	if err := syscall.SetNonblock(lfd, false); err != nil {
		file.Close()
		return s.serveNetpoll(ln)
	}
	// Accepted sockets inherit this on Linux; it is set on each of them as well.
	syscall.SetsockoptInt(lfd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
	ln.Close() // the runtime's poller no longer watches the socket; accept4 below does
	t := &threads{srv: s, lfd: lfd, lfile: file, fds: make(map[int]struct{}, threadsMax)}
	switch {
	case s.Yield == 0:
		t.yield = threadsYield
	case s.Yield > 0:
		t.yield = s.Yield
	}
	s.threads.Store(t)
	if s.stopping.Load() {
		t.halt() // Shutdown ran before it could see t
	}
	return t.run()
}

// run is the accept loop. It returns after Server.Shutdown, or if accepting fails for good.
func (t *threads) run() error {
	for {
		// A blocking call the scheduler knows about: while it waits, the P serves someone else.
		// No address is asked for: nothing in the server uses it.
		nfd, _, errno := syscall.Syscall6(syscall.SYS_ACCEPT4, uintptr(t.lfd), 0, 0, syscall.SOCK_CLOEXEC, 0, 0)
		if errno != 0 {
			if t.srv.stopping.Load() {
				t.end()
				return net.ErrClosed
			}
			switch errno {
			case syscall.EINTR, syscall.ECONNABORTED:
			case syscall.EBADF, syscall.EINVAL, syscall.ENOTSOCK, syscall.EOPNOTSUPP, syscall.EFAULT:
				t.end()
				return errno
			default:
				// Out of descriptors or memory, or a network error on the connection that was
				// next: wait a moment and keep serving, as the netpoll loop does.
				time.Sleep(5 * time.Millisecond)
			}
			continue
		}
		fd := int(nfd)
		syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		if st := t.srv.stats; st != nil {
			st.accepted.Add(1)
			if n := t.live.Add(1); n > st.peak.Load() {
				st.peak.Store(n)
			}
		}
		switch t.adopt(fd) {
		case adopted:
			go t.serve(fd)
		case full:
			t.spill(fd)
		case refused:
			syscall.Close(fd)
			t.left()
			continue
		}
		// The new goroutine first, on this thread; see the top of the file.
		runtime.Gosched()
	}
}

const (
	adopted = iota // the connection has a slot and gets a thread
	full           // every slot is taken: the netpoll loop serves it
	refused        // the server is shutting down
)

// adopt gives the connection on fd one of the threadsMax slots, if one is free.
func (t *threads) adopt(fd int) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.halted:
		return refused
	case len(t.fds) >= threadsMax:
		t.spilled++
		return full
	}
	t.fds[fd] = struct{}{}
	t.serving.Add(1)
	return adopted
}

// spill hands the connection on fd to the netpoll loop. The runtime's poller gets a non-blocking
// duplicate of the descriptor (the mode belongs to the socket, not to the descriptor), and fd is
// closed.
func (t *threads) spill(fd int) {
	file := os.NewFile(uintptr(fd), "")
	nc, err := net.FileConn(file)
	file.Close()
	if err != nil {
		t.left()
		return
	}
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetKeepAlive(false) // FileConn turns the probes on; no connection of this server has them
	}
	go func() {
		t.srv.ServeConn(nc)
		t.left()
	}()
}

// left counts a connection out.
func (t *threads) left() {
	if t.srv.stats != nil {
		t.live.Add(-1)
	}
}

// serve is the request loop of one connection with a thread of its own, from the first read to
// closing the descriptor. It is ServeConn on raw descriptors.
func (t *threads) serve(fd int) {
	s := t.srv
	c := s.pool.Get().(*Ctx)
	c.resetConn()
	c.fd = fd
	defer t.release(c)
	var since time.Time // when this goroutine last came from the scheduler, as far as it knows
	if t.yield > 0 {
		since = time.Now()
	}
	for {
		if t.yield > 0 {
			// The clock is read without a system call.
			if now := time.Now(); now.Sub(since) >= t.yield {
				runtime.Gosched()
				since = time.Now()
			}
		}
		// Wait for the client. settle (or a fresh Ctx) left room in the buffer.
		n, errno := blockingRead(fd, c.in[c.end:])
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 || n == 0 {
			return // the socket failed, or the client closed its side: what was complete has been answered
		}
		c.end += n
		s.answer(c)
		if len(c.out) > 0 {
			ok := blockingWrite(fd, c.out)
			c.out = c.out[:0]
			if !ok {
				return
			}
		}
		if c.closeAfter {
			return
		}
		if !c.settle() {
			// The pending request outgrew the limit; its 413 is in c.out and the connection ends.
			blockingWrite(fd, c.out)
			return
		}
	}
}

// release closes a connection, frees its slot and gives its state back for reuse.
func (t *threads) release(c *Ctx) {
	// Under the lock, so that halt never finds the number of a descriptor that is already closed
	// (and may belong to something else by then).
	t.mu.Lock()
	delete(t.fds, c.fd)
	syscall.Close(c.fd)
	t.mu.Unlock()
	t.left()
	c.fd = -1
	c.out = c.out[:0]
	c.shrink()
	t.srv.pool.Put(c)
	t.serving.Done()
}

// halt is Shutdown's part in the threads loop: it wakes the accept loop and every connection's
// goroutine out of their system calls. The loop then ends by itself (end). It may be called more
// than once, and after the loop has ended.
func (t *threads) halt() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.halted = true
	// A listening socket that is shut down fails the accept4 waiting on it (EINVAL).
	syscall.Shutdown(t.lfd, syscall.SHUT_RDWR)
	t.wake()
}

// wake ends every connection that has a thread: a read waiting on a socket that is shut down
// returns 0 and a write fails, and the connection's goroutine closes the descriptor itself. The
// caller holds t.mu.
func (t *threads) wake() {
	for fd := range t.fds {
		syscall.Shutdown(fd, syscall.SHUT_RDWR)
	}
}

// end closes everything the loop owns and waits for its connections' goroutines.
func (t *threads) end() {
	t.mu.Lock()
	t.halted, t.closed = true, true
	t.wake()
	t.lfile.Close()
	t.mu.Unlock()
	t.serving.Wait()
}

// held is the number of connections that have a thread, and spilt the number handed to the netpoll
// loop so far.
func (t *threads) held() (held, spilt int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.fds), t.spilled
}

// blockingRead reads into p, waiting until there is something to read. n == 0 with no error means
// the peer closed its side. It is not a raw call: the scheduler must know that this goroutine may
// be gone for a while.
func blockingRead(fd int, p []byte) (int, syscall.Errno) {
	n, _, errno := syscall.Syscall(syscall.SYS_READ, uintptr(fd), uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)))
	if errno != 0 {
		return 0, errno
	}
	return int(n), 0
}

// blockingWrite writes all of p, waiting for as long as the client takes to read it. A blocking
// send returns short only when a signal interrupts it, and the rest is then sent by another call.
// It reports whether everything was written; false means the connection is broken.
func blockingWrite(fd int, p []byte) bool {
	for len(p) > 0 {
		n, _, errno := syscall.Syscall6(syscall.SYS_SENDTO, uintptr(fd), uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)), msgNoSignal, 0, 0)
		switch errno {
		case 0:
			p = p[n:]
		case syscall.EINTR:
		default:
			return false
		}
	}
	return true
}
