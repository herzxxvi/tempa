//go:build linux

package httpd

import (
	"net"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// The epoll loop, part two: the wait loop, the listener, and the system calls behind the kernel
// interface of reactor.go. Every connection of the server is handled on the one goroutine that
// runs reactor.run.
//
// How it lives with the Go runtime. The loop never blocks in a way the scheduler can see: the wait
// is a raw system call with a short timeout, and every other call is non-blocking. The scheduler
// therefore never hands the processor to another thread behind the loop's back (which would cost a
// futex wake-up per request), and the monitor thread never goes into the deep sleep it has to be
// woken from. The price is that, with one P, other goroutines (the process's life cycle: a poll
// every two seconds) only run when the loop yields, which it does whenever a wait times out and
// every few batches of events otherwise. Their own network I/O is noticed by the runtime's monitor
// thread, within about ten milliseconds.
//
// The one exception is the predictive read (Server.Predict, loop.chase): a read that waits, for at
// most awaitMillis, on the connection the next request is expected on. It is a raw call as well, so
// for those few milliseconds the processor is held just as it is during a wait, and the loop yields
// before every short run of such reads. A read can only wait on a socket in blocking mode, so with
// Predict the accepted sockets are blocking ones and every other read and every write says
// MSG_DONTWAIT, which makes that one call behave exactly as on a non-blocking socket. Without
// Predict nothing changes.

// EpollSupported reports whether this build has the epoll loop.
const EpollSupported = true

const (
	epollWaitMillis = 5                    // upper bound on how long other goroutines wait for the loop to yield
	epollBatch      = 256                  // events taken per wait
	yieldEvery      = 16                   // batches between yields while the loop is busy
	chaseWindow     = 2 * time.Millisecond // predictive reads in a row before the wait is asked again
	acceptBurst     = 64                   // connections accepted per listener event
	msgNoSignal     = 0x4000               // MSG_NOSIGNAL: a write to a closed connection is an error, not a signal
	msgDontWait     = 0x40                 // MSG_DONTWAIT: this call does not wait, whatever the socket's mode
	tcpDeferAccept  = 9                    // TCP_DEFER_ACCEPT
)

// reactor is the loop's operating-system side.
type reactor struct {
	loop
	epfd   int
	lfd    int      // the listening socket (a duplicate of the listener's descriptor)
	lfile  *os.File // owns lfd
	paused bool     // the listener is out of the epoll set
	// Predict: accepted sockets are in blocking mode, and dontWait (MSG_DONTWAIT) goes on every
	// call that must not wait.
	blocking bool
	dontWait uintptr
	// DeferAccept: eager says a connection is accepted only once it has sent something, so it is
	// read at once; probe and inherit are about TCP_NODELAY coming with the listener's socket.
	eager, probe, inherit bool

	events [epollBatch]syscall.EpollEvent
}

// serveEpoll takes over the listener's socket and runs the loop. If the loop cannot be set up, it
// serves with the netpoll loop instead: a server that is up beats a faster one that is not.
func (s *Server) serveEpoll(ln *net.TCPListener) error {
	file, err := ln.File() // a duplicate descriptor; the socket stays open when ln is closed
	if err != nil {
		return s.serveNetpoll(ln)
	}
	lfd := int(file.Fd())
	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		epfd = -1
	}
	// Duplicating put the socket into blocking mode, and the mode is shared with ln's descriptor.
	if nonblock := syscall.SetNonblock(lfd, true); err == nil {
		err = nonblock
	}
	if err == nil {
		event := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(lfd)}
		err = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, lfd, &event)
	}
	if err != nil {
		if epfd >= 0 {
			syscall.Close(epfd)
		}
		file.Close()
		return s.serveNetpoll(ln)
	}
	// Accepted sockets inherit this on Linux; it is set on each of them as well.
	syscall.SetsockoptInt(lfd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
	ln.Close() // the runtime's poller no longer watches the socket; this loop does
	rx := &reactor{epfd: epfd, lfd: lfd, lfile: file}
	rx.loop = loop{srv: s, k: rx, closed: rx.resumeAccept, stats: s.stats}
	if s.Predict {
		rx.blocking, rx.dontWait = true, msgDontWait
		rx.enablePredict(s.predictAfter)
	}
	if s.DeferAccept {
		// One second: a connection that sends nothing is handed over anyway after about that long.
		// A kernel that does not know the option leaves everything as it was.
		rx.eager = syscall.SetsockoptInt(lfd, syscall.IPPROTO_TCP, tcpDeferAccept, 1) == nil
		rx.probe = true
	}
	return rx.run()
}

// run is the wait loop. It returns only after Server.Shutdown, or if the wait itself fails.
func (rx *reactor) run() error {
	// With one P, other goroutines run only when this one yields. With more, they have their own
	// and a yield would only wake an idle thread to look for work.
	yield := runtime.GOMAXPROCS(0) == 1
	batches := 0
	for {
		n, _, errno := syscall.RawSyscall6(syscall.SYS_EPOLL_PWAIT, uintptr(rx.epfd),
			uintptr(unsafe.Pointer(&rx.events[0])), epollBatch, epollWaitMillis, 0, 0)
		if errno != 0 || n == 0 {
			// Nothing to do, or interrupted (the runtime asking this goroutine to make way): the
			// moment for housekeeping and for letting other goroutines run.
			if errno != 0 && errno != syscall.EINTR {
				rx.shutdown()
				return errno
			}
			if rx.srv.stopping.Load() {
				rx.shutdown()
				return net.ErrClosed
			}
			rx.resumeAccept()
			batches = 0
			if yield {
				runtime.Gosched()
			}
			continue
		}
		// Connections first, the listener last: a connection accepted now may get the descriptor
		// number of one closed in this batch, and must not be handed that one's event.
		accept := false
		rx.lone = n == 1
		for i := 0; i < int(n); i++ {
			fd := int(rx.events[i].Fd)
			if fd == rx.lfd {
				accept = true
			} else if c := rx.conn(fd); c != nil {
				rx.ready(c)
			}
		}
		if accept {
			rx.accept()
		}
		if batches++; batches >= yieldEvery && yield {
			batches = 0
			runtime.Gosched()
		}
		if rx.armed() {
			// A window of predictive reads, one after the other for as long as each finds its
			// request. The window is short because nothing else is looked at inside it: when it is
			// over, the next request comes through the wait again, which also reports every other
			// descriptor that became ready meanwhile. So another connection is kept waiting for at
			// most the window plus one read's limit, however busy the predicted one is. The loop
			// yields before each window (no system call when nobody else wants to run), so other
			// goroutines wait no longer than that either; the clock is read without a system call.
			if yield {
				batches = 0
				runtime.Gosched()
			}
			for until := time.Now().Add(chaseWindow); rx.chase() && time.Now().Before(until); {
			}
		}
	}
}

// accept takes the connections waiting on the listener.
func (rx *reactor) accept() {
	for i := 0; i < acceptBurst; i++ {
		flags := uintptr(syscall.SOCK_NONBLOCK | syscall.SOCK_CLOEXEC)
		if rx.blocking {
			flags = syscall.SOCK_CLOEXEC
		}
		// No address is asked for, here or later: nothing in the server uses it.
		nfd, _, errno := syscall.RawSyscall6(syscall.SYS_ACCEPT4, uintptr(rx.lfd), 0, 0, flags, 0, 0)
		switch errno {
		case 0:
			fd := int(nfd)
			if rx.probe {
				// Once: does a connection come with the listener's TCP_NODELAY (Linux: yes)? Then
				// setting it again on every connection is a system call for nothing.
				rx.probe = false
				on, err := syscall.GetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY)
				rx.inherit = err == nil && on != 0
			}
			if !rx.inherit {
				syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
			}
			if rx.stats != nil {
				rx.stats.accepted.Add(1)
			}
			rx.open(fd)
			if rx.eager {
				// Its first bytes are already there (or it waited out the deferral in silence, and
				// the read finds nothing): no need for a wait to say so.
				if c := rx.conn(fd); c != nil {
					rx.ready(c)
				}
			}
		case syscall.EAGAIN:
			return
		case syscall.EINTR, syscall.ECONNABORTED:
		default:
			// Out of descriptors or memory (EMFILE, ENFILE, ENOBUFS, ENOMEM), or an error nobody
			// expects. Stop watching the listener, or the level-triggered wait would report it
			// again at once, forever; it is watched again when a connection closes or the loop
			// next idles.
			rx.pauseAccept()
			return
		}
	}
}

func (rx *reactor) pauseAccept() {
	if !rx.paused {
		rx.paused = true
		syscall.EpollCtl(rx.epfd, syscall.EPOLL_CTL_DEL, rx.lfd, nil)
	}
}

func (rx *reactor) resumeAccept() {
	if !rx.paused {
		return
	}
	event := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(rx.lfd)}
	if err := syscall.EpollCtl(rx.epfd, syscall.EPOLL_CTL_ADD, rx.lfd, &event); err == nil || err == syscall.EEXIST {
		rx.paused = false
	}
}

// shutdown closes everything the loop owns.
func (rx *reactor) shutdown() {
	rx.closed = nil
	rx.endAll()
	syscall.Close(rx.epfd)
	rx.lfile.Close()
}

// The kernel interface. The calls are raw: each returns at once (the sockets are non-blocking), so
// there is nothing for the scheduler to do around them.

func (rx *reactor) read(fd int, p []byte) (int, syscall.Errno) {
	if rx.blocking {
		n, _, errno := syscall.RawSyscall6(syscall.SYS_RECVFROM, uintptr(fd), uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)), msgDontWait, 0, 0)
		if errno != 0 {
			return 0, errno
		}
		return int(n), 0
	}
	n, _, errno := syscall.RawSyscall(syscall.SYS_READ, uintptr(fd), uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)))
	if errno != 0 {
		return 0, errno
	}
	return int(n), 0
}

// patient bounds every waiting read on fd (SO_RCVTIMEO). Reads with MSG_DONTWAIT are not affected.
func (rx *reactor) patient(fd int) bool {
	if !rx.blocking {
		return false
	}
	limit := syscall.Timeval{Usec: awaitMillis * 1000}
	return syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &limit) == nil
}

// await is a plain read on a blocking socket whose reads give up after awaitMillis (patient): it
// returns with the request as soon as that arrives, EAGAIN when the time is up, EINTR on a signal.
func (rx *reactor) await(fd int, p []byte) (int, syscall.Errno) {
	n, _, errno := syscall.RawSyscall(syscall.SYS_READ, uintptr(fd), uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)))
	if errno != 0 {
		return 0, errno
	}
	return int(n), 0
}

func (rx *reactor) send(fd int, p []byte) (int, syscall.Errno) {
	n, _, errno := syscall.RawSyscall6(syscall.SYS_SENDTO, uintptr(fd), uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)), msgNoSignal|rx.dontWait, 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(n), 0
}

func (rx *reactor) watch(fd int, add, writable bool) bool {
	event := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)}
	if writable {
		event.Events = syscall.EPOLLOUT
	}
	op := syscall.EPOLL_CTL_MOD
	if add {
		op = syscall.EPOLL_CTL_ADD
	}
	return syscall.EpollCtl(rx.epfd, op, fd, &event) == nil
}

func (rx *reactor) close(fd int) {
	syscall.Close(fd) // also takes the descriptor out of the epoll set
}
