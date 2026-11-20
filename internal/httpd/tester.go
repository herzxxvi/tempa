package httpd

// Tester runs requests through the engine without a socket: the same parser, dispatch and response
// writer as a live connection, minus the reads and writes. It is for tests and for the allocation
// benchmarks of hot paths in other packages.
type Tester struct {
	srv *Server
	c   *Ctx
}

// NewTester returns a Tester that behaves like one connection to srv.
func NewTester(srv *Server) *Tester {
	srv.init()
	c := srv.pool.Get().(*Ctx)
	c.resetConn()
	return &Tester{srv: srv, c: c}
}

// Do feeds raw, which must hold one or more complete requests, to the engine and returns the bytes
// it would have written. The result is only valid until the next call.
func (t *Tester) Do(raw []byte) []byte {
	c := t.c
	c.out = c.out[:0]
	for len(raw) > 0 {
		used, code := c.parse(raw)
		if code == needMore {
			panic("httpd: Tester.Do needs complete requests")
		}
		if code != 0 {
			c.replied, c.extra = false, c.extra[:0]
			c.JSON(code, errorBody(code))
			break
		}
		t.srv.dispatch(c)
		c.scanned, c.need, c.sent100, c.wantContinue = 0, 0, false, false
		raw = raw[used:]
	}
	c.closeAfter = false
	return c.out
}
