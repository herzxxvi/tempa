package httpd

import (
	"strconv"
	"sync/atomic"
	"syscall"
)

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
	// patient prepares fd for await: from now on a waiting read on it gives up after awaitMillis.
	// It is called once per connection, before its first await, and reports whether that holds;
	// await is never called on a descriptor it refused.
	patient(fd int) bool
	// await is a read that waits for data, for at most awaitMillis: EAGAIN when nothing came in
	// that time, EINTR when the wait was interrupted. It is the one call that may block.
	await(fd int, p []byte) (n int, errno syscall.Errno)
}

// loop is the connection table and the per-connection state machine of the epoll loop.
type loop struct {
	srv    *Server
	k      kernel
	conns  []*Ctx // by descriptor
	closed func() // called after a connection was closed (a slot became free)

	// Counters for Server.StatsLine; nil unless Server.Count is set.
	stats *ioStats
	live  int // open connections (kept only with stats)

	// The hybrid loop (IOHybrid): lend, when set, is offered a connection whose exchange just now
	// extended a sequential streak of at least lendAfter, and may take it out of the loop to serve it
	// on a goroutine of its own (hybrid_linux.go). A connection it took is no longer in conns.
	lend      func(c *Ctx)
	lendAfter int

	// The sequential streak, kept when track is set: by the predictive read and by the hybrid loop.
	track bool

	// The predictive read (Server.Predict); see chase.
	predict    bool
	lone       bool // the wait being handled reported exactly one descriptor
	last, prev int  // the connections of the last two exchanges, newest first
	streak     int  // exchanges in a row that came alone and on the connection of the one before last
	after      int  // the streak that arms the predictive read to begin with (predictAfter)
	want       int  // the streak that arms it now: after, or more following misses
	calm       int  // predictive reads that found their request, since the last one that did not
}

// The predictive read's tuning.
const (
	awaitMillis  = 3    // how long a predictive read waits for its request
	predictAfter = 32   // exchanges that must follow the pattern before the first predictive read
	predictMost  = 1024 // the longest streak that is asked for after repeated misses
	predictCalm  = 256  // hits in a row after which the requirement is back where it started
)

// ioStats is what FORGE_STATS reports. The loop's goroutine writes, the exit path reads.
type ioStats struct {
	accepted, requests, peak, again, hits, misses atomic.Int64
}

func (st *ioStats) line(io string) string {
	n := func(v *atomic.Int64) string { return strconv.FormatInt(v.Load(), 10) }
	return "stats: io=" + io + " accepted=" + n(&st.accepted) + " requests=" + n(&st.requests) +
		" max_conns=" + n(&st.peak) + " read_eagain=" + n(&st.again) +
		" predict_hits=" + n(&st.hits) + " predict_misses=" + n(&st.misses)
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
	l.streak = 0 // the set of connections changed: the pattern has to show again
	if l.stats != nil {
		l.live++
		if int64(l.live) > l.stats.peak.Load() {
			l.stats.peak.Store(int64(l.live))
		}
	}
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
		} else if l.stats != nil && errno == syscall.EAGAIN {
			l.stats.again.Add(1)
		}
		return
	}
	if n == 0 {
		l.end(c) // the client closed its side; everything it sent has been answered
		return
	}
	if !l.track {
		l.took(c, n)
		return
	}
	fd := c.fd
	whole := l.took(c, n)
	l.note(fd, whole)
	if whole && l.lend != nil && l.streak >= l.lendAfter {
		l.lend(c)
	}
}

// adopt puts a connection that was served outside the loop back into the table. The caller has
// made sure that the wait reports it again.
func (l *loop) adopt(c *Ctx) {
	fd := c.fd
	for fd >= len(l.conns) {
		l.conns = append(l.conns, make([]*Ctx, max(64, len(l.conns)))...)
	}
	l.conns[fd] = c
	l.streak = 0 // the set of connections changed: the pattern has to show again
	if l.stats != nil {
		l.live++
	}
}

// part takes a connection out of the table without closing it: from now on it is served outside
// the loop, which must not touch it until adopt.
func (l *loop) part(c *Ctx) {
	l.conns[c.fd] = nil
	l.streak = 0
	if l.stats != nil {
		l.live--
	}
}

// took handles n bytes just read into c: every complete request is answered and the replies are
// written. It reports whether that was a whole exchange and nothing else: something was answered
// and sent in full, and the connection is still open with nothing pending in either direction.
func (l *loop) took(c *Ctx, n int) bool {
	fd := c.fd
	c.end += n
	l.srv.answer(c)
	if len(c.out) == 0 {
		l.settled(c)
		return false
	}
	if !l.flush(c) {
		return false
	}
	l.settled(c)
	return l.conn(fd) == c && c.end == 0 && len(c.out) == 0
}

// note records the outcome of one read for the predictive read: fd's connection had an exchange
// (whole says whether it was a complete one). The pattern looked for is the one sequential clients
// produce: one request at a time, each on the connection of the request before the last one. One
// connection used over and over fits it, and so do two used in turn.
func (l *loop) note(fd int, whole bool) {
	if !whole {
		l.streak = 0
		return
	}
	if l.lone && fd == l.prev {
		l.streak++
	} else {
		l.streak = 0
	}
	l.prev, l.last = l.last, fd
}

// enablePredict switches the predictive read on. after is the streak that arms it; 0 means
// predictAfter (tests arm it sooner).
func (l *loop) enablePredict(after int) {
	if after <= 0 {
		after = predictAfter
	}
	l.predict, l.track, l.after, l.want, l.last, l.prev = true, true, after, after, -1, -1
}

// armed reports whether the next request is expected on a known connection, so that chase may wait
// for it there.
func (l *loop) armed() bool { return l.predict && l.streak >= l.want }

// chase is the predictive read. Once the last predictAfter exchanges have followed the pattern of
// note, the next request is expected on the connection l.prev, and instead of asking the wait which
// descriptor is ready and then reading it, chase reads that connection with a read that waits: one
// system call instead of two. It handles one such read and reports whether to go on with another.
//
// What it costs when the guess is wrong: the read gives up after awaitMillis, and for that long
// nothing else is served (another connection's request, a new connection). So a miss doubles the
// streak asked for before the next attempt, up to predictMost exchanges, and a long run of hits
// brings it back down. A read that fails in any way hands over to the ordinary wait, which finds
// whatever is ready; no byte is read twice or skipped, because the read is the same read either way.
func (l *loop) chase() bool {
	c := l.conn(l.prev)
	if c == nil || c.blocked || c.end != 0 || len(c.out) != 0 {
		l.streak = 0
		return false
	}
	fd := c.fd
	if !c.patient {
		if !l.k.patient(fd) {
			l.predict = false // a read that cannot be bounded is never made
			return false
		}
		c.patient = true
	}
	n, errno := l.k.await(fd, c.in)
	if errno != 0 {
		switch errno {
		case syscall.EINTR:
			// The runtime wants the processor. No verdict on the guess: the wait takes over for
			// this exchange, and the streak goes on if the request comes where it was expected.
		case syscall.EAGAIN:
			l.streak, l.calm = 0, 0
			l.want = min(2*l.want, predictMost)
			if l.stats != nil {
				l.stats.misses.Add(1)
			}
		default:
			l.end(c)
		}
		return false
	}
	if n == 0 {
		l.end(c)
		return false
	}
	if l.calm++; l.calm >= predictCalm {
		l.want = l.after
	}
	if l.stats != nil {
		l.stats.hits.Add(1)
	}
	l.lone = true
	l.note(fd, l.took(c, n))
	return l.streak >= l.want
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
	l.streak = 0
	if l.stats != nil {
		l.live--
	}
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
