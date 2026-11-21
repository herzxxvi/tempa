//go:build linux

package httpd

import (
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// The hybrid loop: the epoll loop of reactor_linux.go, which lends a connection that is clearly
// being used alone, one request after the other, to a goroutine that serves it the way the threads
// loop does (threads_linux.go), and takes it back when it goes quiet.
//
// Why. In the epoll loop an exchange is three system calls (wait, read, write) and in the threads
// loop two (a read that waits, write). But many connections that are busy at once are cheaper in
// the epoll loop, where one wait reports all that are ready, than on a thread each. So the loop
// serves everything, and only the traffic that gains from it leaves the loop.
//
// Lending (promote). The loop keeps the sequential streak of loop.note: exchanges that each came
// alone in their wait, complete, on the connection of the exchange before the last one. When that
// streak reaches the number asked for (hybridAfter to begin with) the connection of the exchange
// just finished, which has nothing pending in either direction, is taken out of the wait's set and
// out of the loop's table and handed to a goroutine. At most hybridMax connections are out at a
// time; beyond that everything stays in the loop.
//
// The sockets of this loop are in blocking mode from the start, as with Server.Predict: the loop
// says MSG_DONTWAIT on every call, which makes each behave exactly as on a non-blocking socket,
// and the goroutine waits with plain reads. Its reads give up after hybridIdleMillis (SO_RCVTIMEO,
// set once per connection; calls with MSG_DONTWAIT are not affected by it).
//
// Giving back (giveBack). A read that times out sends the connection back. At that point it is
// exactly where the loop leaves a connection before a read: everything received so far has been
// answered and written, and what is left of an incomplete request sits at the start of the input
// buffer. The goroutine puts the connection on the list of returned ones and adds the descriptor
// to the wait's set again, both under the mutex, and never touches it afterwards. The loop does
// not hear of this until the wait reports the descriptor: it then finds no connection in its
// table, asks the list (reclaim), and goes on as with any other connection. Nothing is lost on the
// way: the descriptor is on the list before the wait can report it, readiness is level-triggered,
// and every byte is read by whoever owns the connection at that moment into the one buffer that
// travels with it.
//
// Ownership. A connection belongs either to the loop (it is in loop.conns), or to one goroutine
// (its descriptor is in hybrid.fds), or to nobody yet (it is in hybrid.back); the changes between
// the last two happen under hybrid.mu, and the loop takes from back under the same mutex. No
// connection ever waits for another one's I/O: a goroutine blocks only on its own socket, and the
// loop never blocks on a socket at all.
//
// How it lives with the Go runtime. See threads_linux.go for what a goroutine in a blocking system
// call needs: idle Ps (the process runs this loop with GOMAXPROCS of 4 or more) and a pass through
// the scheduler every few milliseconds (Server.Yield). The loop's own goroutine gets the same
// treatment here: while a connection is out, or when a wait has found nothing, it waits in a long
// epoll wait that the scheduler knows about instead of polling every 5 ms, so an idle loop costs
// nothing; and while it is busy it passes through the scheduler every hybridYield.
//
// Shutdown. halt (from Server.Shutdown) shuts down the sockets that are out, which ends their
// goroutines' reads and writes, and wakes the loop through an eventfd; the loop then waits for
// those goroutines (stop) and closes everything else, the returned connections included.

// HybridSupported reports whether this build has the hybrid loop.
const HybridSupported = true

const (
	hybridAfter      = 8                    // the sequential streak that gets a connection lent out
	hybridMost       = 512                  // the longest streak that is asked for after short stays
	hybridMax        = 4                    // the most connections that are out at one time
	hybridIdleMillis = 20                   // how long a lent connection may be silent before it goes back
	hybridWaitMillis = 1000                 // the loop's wait while it has nothing to do
	hybridYield      = 5 * time.Millisecond // the busy loop passes through the scheduler this often
	efdCloexec       = 0x80000              // EFD_CLOEXEC
	efdNonblock      = 0x800                // EFD_NONBLOCK
)

// hybrid is what the loop and the goroutines of lent connections share.
type hybrid struct {
	srv   *Server
	epfd  int           // the loop's epoll descriptor
	wfd   int           // an eventfd in the loop's set, written by halt; -1 if there is none
	yield time.Duration // Server.Yield, as in the threads loop; 0: never
	after int32         // the streak asked for to begin with: hybridAfter (tests ask for less)

	away    atomic.Int32 // connections that are out
	waiting atomic.Int32 // len(back), for the loop to ask without the mutex
	need    atomic.Int32 // the streak asked for now: hybridAfter, or more following short stays
	lent    atomic.Int64 // connections lent out so far
	given   atomic.Int64 // connections given back so far

	mu      sync.Mutex
	fds     map[int]struct{} // the connections that are out, by descriptor
	back    []*Ctx           // given back, not yet in the loop's table
	halted  bool             // Shutdown was called: nothing is lent or given back any more
	closed  bool             // the loop has ended; wfd is no longer ours
	serving sync.WaitGroup   // the goroutines of the connections in fds
}

func (s *Server) serveHybrid(ln *net.TCPListener) error { return s.serveLoop(ln, true) }

func newHybrid(s *Server, epfd int) *hybrid {
	h := &hybrid{srv: s, epfd: epfd, wfd: -1, fds: make(map[int]struct{}, hybridMax)}
	h.after = hybridAfter
	if s.hybridAfter > 0 {
		h.after = int32(s.hybridAfter)
	}
	h.need.Store(h.after)
	switch {
	case s.Yield == 0:
		h.yield = threadsYield
	case s.Yield > 0:
		h.yield = s.Yield
	}
	// Without the eventfd the loop still notices Shutdown, when its wait next returns.
	if fd, _, errno := syscall.RawSyscall(syscall.SYS_EVENTFD2, 0, efdCloexec|efdNonblock, 0); errno == 0 {
		event := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)}
		if syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, int(fd), &event) == nil {
			h.wfd = int(fd)
		} else {
			syscall.Close(int(fd))
		}
	}
	return h
}

// promote is loop.lend: the loop offers c, whose exchange just now was a complete one that
// extended the sequential streak, and c is still open with nothing pending in either direction.
// It runs on the loop's goroutine.
func (rx *reactor) promote(c *Ctx) {
	h := rx.hy
	if rx.streak < int(h.need.Load()) || h.away.Load() >= hybridMax {
		return
	}
	fd := c.fd
	if !c.patient {
		limit := syscall.Timeval{Usec: hybridIdleMillis * 1000}
		if syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &limit) != nil {
			rx.lend = nil // a read that cannot be bounded is never made: everything stays in the loop
			return
		}
		c.patient = true
	}
	if !h.take(fd) {
		return
	}
	if syscall.EpollCtl(rx.epfd, syscall.EPOLL_CTL_DEL, fd, nil) != nil {
		h.drop(fd)
		return
	}
	rx.part(c)
	h.lent.Add(1)
	go h.serve(c)
	// The new goroutine first, on this thread, as in the threads loop: the loop's goroutine moves
	// to an idle P instead of going into its wait with the new one queued behind it.
	runtime.Gosched()
}

// take gives the connection on fd one of the hybridMax places, if one is free.
func (h *hybrid) take(fd int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.halted || len(h.fds) >= hybridMax {
		return false
	}
	h.fds[fd] = struct{}{}
	h.serving.Add(1)
	h.away.Add(1)
	return true
}

// drop undoes take for a connection that stays in the loop after all.
func (h *hybrid) drop(fd int) {
	h.mu.Lock()
	delete(h.fds, fd)
	h.mu.Unlock()
	h.away.Add(-1)
	h.serving.Done()
}

// serve is the request loop of a lent connection: threads.serve, with a read that gives up. It
// ends by giving the connection back to the loop, or by closing it.
func (h *hybrid) serve(c *Ctx) {
	s, fd := h.srv, c.fd
	served := 0         // exchanges while the connection was out
	var since time.Time // when this goroutine last came from the scheduler, as far as it knows
	if h.yield > 0 {
		since = time.Now()
	}
	for {
		if h.yield > 0 {
			// The clock is read without a system call.
			if now := time.Now(); now.Sub(since) >= h.yield {
				runtime.Gosched()
				since = time.Now()
			}
		}
		// Wait for the client. settle (or the loop, before it lent the connection) left room in
		// the buffer.
		n, errno := blockingRead(fd, c.in[c.end:])
		switch {
		case errno == syscall.EINTR:
			continue
		case errno == syscall.EAGAIN: // also EWOULDBLOCK: the client was silent for hybridIdleMillis
			h.giveBack(c, served)
			return
		case errno != 0 || n == 0:
			h.release(c) // the socket failed, or the client closed its side: what was complete has been answered
			return
		}
		c.end += n
		s.answer(c)
		if len(c.out) > 0 {
			ok := blockingWrite(fd, c.out)
			c.out = c.out[:0]
			if !ok {
				h.release(c)
				return
			}
		}
		if c.closeAfter {
			h.release(c)
			return
		}
		if !c.settle() {
			// The pending request outgrew the limit; its 413 is in c.out and the connection ends.
			blockingWrite(fd, c.out)
			h.release(c)
			return
		}
		served++
	}
}

// giveBack returns a silent connection to the loop. After a stay that was not worth the two
// changes of hands, the streak asked for doubles (up to hybridMost); after one that was, it is
// back at hybridAfter.
func (h *hybrid) giveBack(c *Ctx, served int) {
	if served < int(h.after) {
		h.need.Store(min(2*h.need.Load(), hybridMost))
	} else {
		h.need.Store(h.after)
	}
	fd := c.fd
	h.mu.Lock()
	if h.halted {
		h.mu.Unlock()
		h.release(c)
		return
	}
	// On the list first, then into the wait's set: the loop asks the list when the wait reports a
	// descriptor it has no connection for, and to ask it waits for this mutex.
	h.back = append(h.back, c)
	h.waiting.Add(1)
	event := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)}
	if syscall.EpollCtl(h.epfd, syscall.EPOLL_CTL_ADD, fd, &event) != nil {
		h.back = h.back[:len(h.back)-1]
		h.waiting.Add(-1)
		h.mu.Unlock()
		h.release(c) // the loop cannot watch it: there is nobody left to serve it
		return
	}
	delete(h.fds, fd)
	h.mu.Unlock()
	// From here on the connection is the loop's.
	h.given.Add(1)
	h.away.Add(-1)
	h.serving.Done()
}

// reclaim hands the loop the returned connection on fd, or nil when there is none. It runs on the
// loop's goroutine.
func (h *hybrid) reclaim(fd int) *Ctx {
	if h.waiting.Load() == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, c := range h.back {
		if c.fd == fd {
			last := len(h.back) - 1
			h.back[i] = h.back[last]
			h.back[last] = nil
			h.back = h.back[:last]
			h.waiting.Add(-1)
			return c
		}
	}
	return nil
}

// release closes a lent connection, frees its place and gives its state back for reuse.
func (h *hybrid) release(c *Ctx) {
	// Under the lock, so that halt never finds the number of a descriptor that is already closed
	// (and may belong to something else by then).
	h.mu.Lock()
	delete(h.fds, c.fd)
	syscall.Close(c.fd)
	h.mu.Unlock()
	c.fd = -1
	c.out, c.written, c.blocked = c.out[:0], 0, false
	c.shrink()
	h.srv.pool.Put(c)
	h.away.Add(-1)
	h.serving.Done()
}

// halt is Shutdown's part in the hybrid loop: it wakes the goroutines of the lent connections out
// of their system calls (a read on a socket that is shut down returns 0, a write fails), and the
// loop out of its wait. It may be called more than once, and after the loop has ended.
func (h *hybrid) halt() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.halted = true
	for fd := range h.fds {
		syscall.Shutdown(fd, syscall.SHUT_RDWR)
	}
	if h.wfd >= 0 {
		one := uint64(1)
		syscall.RawSyscall(syscall.SYS_WRITE, uintptr(h.wfd), uintptr(unsafe.Pointer(&one)), 8)
	}
}

// stop ends everything that is outside the loop, for the loop's own shutdown: it waits for the
// goroutines of the lent connections, which close them, and returns the connections that were
// given back and never reclaimed, which the caller closes. It runs on the loop's goroutine, so
// nothing is lent meanwhile.
func (h *hybrid) stop() []*Ctx {
	h.halt()
	h.serving.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	back := h.back
	h.back = nil
	h.waiting.Store(0)
	h.closed = true
	if h.wfd >= 0 {
		syscall.Close(h.wfd)
		h.wfd = -1
	}
	return back
}

// held is the number of connections that are out now, and how many were lent and given back so
// far.
func (h *hybrid) held() (out int, lent, given int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.fds), h.lent.Load(), h.given.Load()
}

// statsTail is the hybrid loop's part of Server.StatsLine.
func (h *hybrid) statsTail() string {
	return " lent=" + strconv.FormatInt(h.lent.Load(), 10) + " given_back=" + strconv.FormatInt(h.given.Load(), 10)
}
