package httpd

import "syscall"

// The epoll loop, part one: what happens to a connection when the operating system says it is
// ready. This half has no system calls in it: it talks to the kernel through a small interface, so
// that it can be driven by a scripted kernel in tests on any platform. The other half
// (reactor_linux.go) is the wait loop, the listener and the real system calls.
//
// Why an epoll loop at all. With Go's net package an exchange on a persistent connection costs
// about four and a half system calls: the read, the write, a second read that only learns there is
// nothing more (EAGAIN), and the runtime's own epoll polling. Where every system call is expensive
// (a sandboxed kernel), that is most of the server's cost. The loop needs three: one wait, one
// read, one write; and when several connections are ready at once, the wait is shared by all.
//
// How it stays correct.
//   - Readiness is level-triggered: a connection with unread bytes is reported again by the next
//     wait, so reading once per event can never strand a request, and a request that arrives while
//     another is being handled is found by the next wait.
//   - Requests are handled by the same code as in the netpoll loop (Server.answer, Ctx.settle):
//     every complete request in the buffer is answered, in order, and the responses leave together.
//   - A write the socket does not take in full leaves the rest in the connection's output buffer.
//     The connection then waits for writability instead of readability, so nothing more is read,
//     and nothing more is buffered, until the client has taken what it was sent.

// kernel is what the loop needs from the operating system for an established connection. Every
// call is non-blocking.
type kernel interface {
	// read reads into p. n == 0 with no error means the peer closed its side.
	read(fd int, p []byte) (n int, errno syscall.Errno)
	// send writes p and returns how much was taken; EAGAIN when the socket takes nothing now.
	send(fd int, p []byte) (n int, errno syscall.Errno)
	// watch sets what the wait reports for fd: readable, or (writable set) writable. add is true
	// for a descriptor not yet watched. It reports whether the kernel accepted the request.
	watch(fd int, add, writable bool) bool
	// close closes fd, which also ends the watch.
	close(fd int)
}

// loop is the connection table and the per-connection state machine of the epoll loop.
type loop struct {
	srv    *Server
	k      kernel
	conns  []*Ctx // by descriptor
	closed func() // called after a connection was closed (a slot became free)
}

// maxDescriptor bounds the connection table; a descriptor beyond it is refused.
const maxDescriptor = 1 << 20

// conn returns the connection on fd, or nil. An event for a descriptor that was closed earlier in
// the same batch of events finds nil here and is dropped.
func (l *loop) conn(fd int) *Ctx {
	if fd >= 0 && fd < len(l.conns) {
		return l.conns[fd]
	}
	return nil
}

// open adopts a freshly accepted connection and starts watching it for requests.
func (l *loop) open(fd int) {
	if fd < 0 || fd >= maxDescriptor {
		l.k.close(fd)
		return
	}
	c := l.srv.pool.Get().(*Ctx)
	c.resetConn()
	c.fd = fd
	for fd >= len(l.conns) {
		l.conns = append(l.conns, make([]*Ctx, max(64, len(l.conns)))...)
	}
	l.conns[fd] = c
	if !l.k.watch(fd, true, false) {
		l.end(c)
	}
}

// ready handles a connection the wait reported. What the connection is waiting for decides what is
// done; an error or hang-up on the socket surfaces as a failed read or write, which closes it.
func (l *loop) ready(c *Ctx) {
	if c.blocked {
		if l.flush(c) {
			l.settled(c)
		}
		return
	}
	if c.end == len(c.in) && !c.room() {
		c.refuse(413)
		if l.flush(c) {
			l.end(c)
		}
		return
	}
	n, errno := l.k.read(c.fd, c.in[c.end:])
	if errno != 0 {
		if errno != syscall.EAGAIN && errno != syscall.EINTR {
			l.end(c)
		}
		return
	}
	if n == 0 {
		l.end(c) // the client closed its side; everything it sent has been answered
		return
	}
	c.end += n
	l.srv.answer(c)
	if len(c.out) == 0 || l.flush(c) {
		l.settled(c)
	}
}

// flush writes what is pending in c.out. It returns true when all of it is gone. When the socket
// will not take it all, the connection is switched to wait for writability and flush returns false;
// on a write error the connection is closed and flush returns false.
func (l *loop) flush(c *Ctx) bool {
	for c.written < len(c.out) {
		n, errno := l.k.send(c.fd, c.out[c.written:])
		switch errno {
		case 0:
			c.written += n
		case syscall.EINTR:
		case syscall.EAGAIN:
			if !c.blocked {
				c.blocked = true
				l.k.watch(c.fd, false, true)
			}
			return false
		default:
			l.end(c)
			return false
		}
	}
	c.out, c.written = c.out[:0], 0
	if c.blocked {
		c.blocked = false
		l.k.watch(c.fd, false, false)
	}
	return true
}

// settled runs once a connection's output is fully written: close it if that was the last reply,
// otherwise get ready for the next read.
func (l *loop) settled(c *Ctx) {
	if c.closeAfter {
		l.end(c)
		return
	}
	if !c.settle() {
		// The pending request outgrew the limit; its 413 is in c.out and the connection ends.
		if l.flush(c) {
			l.end(c)
		}
	}
}

// end closes a connection and gives its state back for reuse.
func (l *loop) end(c *Ctx) {
	fd := c.fd
	if l.conn(fd) != c {
		return
	}
	l.conns[fd] = nil
	l.k.close(fd)
	c.fd = -1
	c.out, c.written, c.blocked = c.out[:0], 0, false
	c.shrink()
	l.srv.pool.Put(c)
	if l.closed != nil {
		l.closed()
	}
}

// endAll closes every connection.
func (l *loop) endAll() {
	for _, c := range l.conns {
		if c != nil {
			l.end(c)
		}
	}
}
