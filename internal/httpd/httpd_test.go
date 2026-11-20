package httpd

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// echo is the test handler: it reports what the engine parsed, as JSON-ish text.
func echo(c *Ctx) {
	switch string(c.Path) {
	case "/panic":
		panic("boom")
	case "/silent":
		return
	case "/empty":
		c.Empty(204)
		return
	case "/git":
		c.Bytes(200, "application/x-git-upload-pack-advertisement", []byte("0000"))
		return
	case "/link":
		c.AddHeader("Link", "<http://x/next>; rel=\"next\"")
	}
	b := c.Scratch[:0]
	b = append(b, `{"method":`...)
	b = strconv.AppendInt(b, int64(c.Method), 10)
	b = append(b, `,"path":"`...)
	b = append(b, c.Path...)
	b = append(b, `","query":"`...)
	b = append(b, c.Query...)
	b = append(b, `","host":"`...)
	b = append(b, c.Host...)
	b = append(b, `","body":"`...)
	b = append(b, c.Body...)
	b = append(b, `"}`...)
	c.Scratch = b
	c.JSON(200, b)
}

type response struct {
	status int
	header map[string]string
	body   string
}

type client struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func start(t *testing.T, h Handler) (*Server, *client) {
	t.Helper()
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Handler: h, Token: "secret", DefaultHost: "fallback:1"}
	go srv.Serve(ln)
	t.Cleanup(func() { ln.Close() })
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	return srv, &client{t: t, conn: conn, r: bufio.NewReader(conn)}
}

func (c *client) send(raw string) {
	c.t.Helper()
	if _, err := io.WriteString(c.conn, raw); err != nil {
		c.t.Fatal(err)
	}
}

// read parses one response off the connection. hasBody is false for replies to HEAD.
func (c *client) read(hasBody bool) response {
	c.t.Helper()
	res := response{header: map[string]string{}}
	line, err := c.r.ReadString('\n')
	if err != nil {
		c.t.Fatalf("reading status line: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 ") || !strings.HasSuffix(line, "\r\n") {
		c.t.Fatalf("bad status line %q", line)
	}
	res.status, _ = strconv.Atoi(line[9:12])
	for {
		line, err = c.r.ReadString('\n')
		if err != nil {
			c.t.Fatalf("reading headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
		name, value, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), ": ")
		if _, dup := res.header[strings.ToLower(name)]; dup {
			c.t.Fatalf("header %s sent twice", name)
		}
		res.header[strings.ToLower(name)] = value
	}
	if n, err := strconv.Atoi(res.header["content-length"]); err == nil && hasBody && res.status != 100 {
		body := make([]byte, n)
		if _, err := io.ReadFull(c.r, body); err != nil {
			c.t.Fatalf("reading body: %v", err)
		}
		res.body = string(body)
	}
	return res
}

func (c *client) expectClosed() {
	c.t.Helper()
	if _, err := c.r.ReadByte(); err != io.EOF {
		c.t.Fatalf("connection should be closed, got %v", err)
	}
}

func TestKeepAliveAndHeaders(t *testing.T) {
	_, c := start(t, echo)
	for i := 0; i < 3; i++ {
		c.send("GET /a/b?x=1&y=2 HTTP/1.1\r\nhost: example.test:8081\r\nAccept: */*\r\n\r\n")
		res := c.read(true)
		if res.status != 200 || res.body != `{"method":1,"path":"/a/b","query":"x=1&y=2","host":"example.test:8081","body":""}` {
			t.Fatalf("request %d: %d %s", i, res.status, res.body)
		}
		if res.header["content-type"] != "application/json; charset=utf-8" || len(res.header["x-github-request-id"]) != 16 {
			t.Fatalf("headers: %v", res.header)
		}
		if _, has := res.header["connection"]; has {
			t.Fatal("a persistent connection must not send a Connection header")
		}
		if _, has := res.header["date"]; has {
			t.Fatal("no Date header is sent")
		}
	}
}

func TestRequestIDsAreUnique(t *testing.T) {
	_, c := start(t, echo)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		c.send("GET / HTTP/1.1\r\nHost: h\r\n\r\n")
		id := c.read(true).header["x-github-request-id"]
		if id == "" || seen[id] {
			t.Fatalf("request id %q empty or repeated", id)
		}
		seen[id] = true
	}
}

func TestPipelinedRequestsAnswerInOrder(t *testing.T) {
	_, c := start(t, echo)
	c.send("GET /1 HTTP/1.1\r\nHost: h\r\n\r\n" +
		"POST /2 HTTP/1.1\r\nHost: h\r\nContent-Length: 3\r\n\r\nabc" +
		"\r\nDELETE /3 HTTP/1.1\r\nHost: h\r\nContent-Length: 2\r\n\r\nxy")
	for i, want := range []string{`"path":"/1","query":"","host":"h","body":""`, `"path":"/2","query":"","host":"h","body":"abc"`, `"path":"/3","query":"","host":"h","body":"xy"`} {
		if res := c.read(true); res.status != 200 || !strings.Contains(res.body, want) {
			t.Fatalf("response %d: %d %s", i+1, res.status, res.body)
		}
	}
}

func TestBodyArrivingInPieces(t *testing.T) {
	_, c := start(t, echo)
	c.send("POST /p HTTP/1.1\r\nHo")
	time.Sleep(20 * time.Millisecond)
	c.send("st: h\r\nContent-Length: 10\r\n\r\n01234")
	time.Sleep(20 * time.Millisecond)
	c.send("56789")
	if res := c.read(true); !strings.Contains(res.body, `"body":"0123456789"`) {
		t.Fatalf("body: %s", res.body)
	}
}

func TestLargeBodyGrowsTheBuffer(t *testing.T) {
	_, c := start(t, func(c *Ctx) {
		c.Scratch = strconv.AppendInt(c.Scratch[:0], int64(len(c.Body)), 10)
		if len(c.Body) > 0 && (c.Body[0] != 'a' || c.Body[len(c.Body)-1] != 'z') {
			c.Scratch = append(c.Scratch, " corrupted"...)
		}
		c.JSON(200, c.Scratch)
	})
	for _, size := range []int{inputSize - 100, inputSize + 1, 300 << 10, 5} {
		body := "a" + strings.Repeat("m", size-2) + "z"
		go c.send("POST /big HTTP/1.1\r\nHost: h\r\nContent-Length: " + strconv.Itoa(size) + "\r\n\r\n" + body)
		if res := c.read(true); res.body != strconv.Itoa(size) {
			t.Fatalf("size %d: server saw %q", size, res.body)
		}
	}
}

func TestChunkedBody(t *testing.T) {
	_, c := start(t, echo)
	c.send("POST /c HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n")
	time.Sleep(20 * time.Millisecond)
	c.send("A;ext=1\r\n0123456789\r\n0\r\nTrailer: x\r\n\r\nGET /after HTTP/1.1\r\nHost: h\r\n\r\n")
	if res := c.read(true); !strings.Contains(res.body, `"body":"abc0123456789"`) {
		t.Fatalf("chunked body: %s", res.body)
	}
	if res := c.read(true); !strings.Contains(res.body, `"path":"/after"`) {
		t.Fatalf("request after a chunked one: %s", res.body)
	}
}

func TestExpectContinue(t *testing.T) {
	_, c := start(t, echo)
	c.send("POST /e HTTP/1.1\r\nHost: h\r\nExpect: 100-continue\r\nContent-Length: 4\r\n\r\n")
	if res := c.read(false); res.status != 100 {
		t.Fatalf("expected 100 Continue, got %d", res.status)
	}
	c.send("data")
	if res := c.read(true); res.status != 200 || !strings.Contains(res.body, `"body":"data"`) {
		t.Fatalf("after continue: %d %s", res.status, res.body)
	}
}

func TestConnectionCloseAndHTTP10(t *testing.T) {
	_, c := start(t, echo)
	c.send("GET /x HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n")
	if res := c.read(true); res.status != 200 || res.header["connection"] != "close" {
		t.Fatalf("close: %d %v", res.status, res.header)
	}
	c.expectClosed()

	_, c = start(t, echo)
	c.send("GET /x HTTP/1.0\r\n\r\n")
	if res := c.read(true); res.status != 200 || res.header["connection"] != "close" || !strings.Contains(res.body, `"host":"fallback:1"`) {
		t.Fatalf("HTTP/1.0: %d %v %s", res.status, res.header, res.body)
	}
	c.expectClosed()
}

func TestMalformedRequests(t *testing.T) {
	for raw, want := range map[string]int{
		"NONSENSE\r\n\r\n":                                                                             400,
		"GET / HTTP/2.0\r\nHost: h\r\n\r\n":                                                            505,
		"GET / HTTP/1.1\r\nHost : h\r\n\r\n":                                                           400,
		"POST / HTTP/1.1\r\nHost: h\r\nContent-Length: abc\r\n\r\n":                                    400,
		"POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: gzip\r\n\r\n":                                501,
		"GET / HTTP/1.1\r\n" + strings.Repeat("X-Pad: "+strings.Repeat("p", 1000)+"\r\n", 20) + "\r\n": 431,
	} {
		_, c := start(t, echo)
		c.send(raw)
		res := c.read(true)
		if res.status != want || !strings.HasPrefix(res.body, `{"message":"`) || res.header["connection"] != "close" {
			t.Errorf("%q: %d %v %s, want %d", raw[:min(len(raw), 40)], res.status, res.header, res.body, want)
		}
		c.expectClosed()
	}
}

func TestPanicBecomes500AndConnectionSurvives(t *testing.T) {
	_, c := start(t, echo)
	c.send("GET /panic HTTP/1.1\r\nHost: h\r\n\r\nGET /silent HTTP/1.1\r\nHost: h\r\n\r\nGET /ok HTTP/1.1\r\nHost: h\r\n\r\n")
	for i, want := range []int{500, 500, 200} {
		if res := c.read(true); res.status != want {
			t.Fatalf("response %d: status %d, want %d", i, res.status, want)
		}
	}
}

func TestOtherReplyKinds(t *testing.T) {
	_, c := start(t, echo)
	c.send("DELETE /empty HTTP/1.1\r\nHost: h\r\n\r\n")
	res := c.read(true)
	if _, has := res.header["content-length"]; res.status != 204 || has || res.header["x-github-request-id"] == "" {
		t.Fatalf("204: %d %v", res.status, res.header)
	}
	c.send("GET /git HTTP/1.1\r\nHost: h\r\n\r\n")
	res = c.read(true)
	if res.status != 200 || res.body != "0000" || res.header["content-type"] != "application/x-git-upload-pack-advertisement" || len(res.header) != 2 {
		t.Fatalf("git reply: %d %v %q", res.status, res.header, res.body)
	}
	c.send("GET /link HTTP/1.1\r\nHost: h\r\n\r\n")
	if res = c.read(true); res.header["link"] != `<http://x/next>; rel="next"` {
		t.Fatalf("extra header: %v", res.header)
	}
	c.send("GET /plain HTTP/1.1\r\nHost: h\r\n\r\n")
	if res = c.read(true); res.header["link"] != "" {
		t.Fatal("an extra header leaked into the next reply")
	}
	c.send("HEAD /plain HTTP/1.1\r\nHost: h\r\n\r\nGET /next HTTP/1.1\r\nHost: h\r\n\r\n")
	if res = c.read(false); res.status != 200 || res.header["content-length"] == "0" {
		t.Fatalf("HEAD: %d %v", res.status, res.header)
	}
	if res = c.read(true); !strings.Contains(res.body, `"path":"/next"`) {
		t.Fatalf("after HEAD: %s", res.body)
	}
}

func TestTailReplies(t *testing.T) {
	tail := AppendTail(nil, []byte(`{"n":1}`))
	if string(TailBody(tail)) != `{"n":1}` {
		t.Fatalf("TailBody = %q", TailBody(tail))
	}
	whole := AppendResponse(nil, 200, "text/x", []byte("hi"))
	_, c := start(t, func(c *Ctx) {
		if string(c.Path) == "/pre" {
			c.Prebuilt(whole)
			return
		}
		c.JSONTail(201, tail)
	})
	c.send("POST /t HTTP/1.1\r\nHost: h\r\n\r\nGET /pre HTTP/1.1\r\nHost: h\r\n\r\n")
	if res := c.read(true); res.status != 201 || res.body != `{"n":1}` || res.header["content-type"] != "application/json; charset=utf-8" {
		t.Fatalf("tail reply: %d %v %s", res.status, res.header, res.body)
	}
	if res := c.read(true); res.status != 200 || res.body != "hi" || res.header["content-type"] != "text/x" {
		t.Fatalf("prebuilt reply: %d %v %s", res.status, res.header, res.body)
	}
}

func TestAuth(t *testing.T) {
	var got Auth
	_, c := start(t, func(c *Ctx) { got = c.Auth(); c.Empty(204) })
	for header, want := range map[string]Auth{
		"":                                      AuthNone,
		"Authorization: Bearer secret\r\n":      AuthOK,
		"authorization: bearer   secret\r\n":    AuthOK,
		"Authorization: token secret\r\n":       AuthOK,
		"Authorization: TOKEN secret\r\n":       AuthOK,
		"Authorization: Basic eDpzZWNyZXQ=\r\n": AuthOK, // x:secret
		"Authorization: Bearer wrong\r\n":       AuthBad,
		"Authorization: Bearer secret2\r\n":     AuthBad,
		"Authorization: Bearer\r\n":             AuthBad,
		"Authorization: secret\r\n":             AuthBad,
		"Authorization: Digest secret\r\n":      AuthBad,
		"Authorization: Basic !!!\r\n":          AuthBad,
	} {
		c.send("GET / HTTP/1.1\r\nHost: h\r\n" + header + "\r\n")
		c.read(true)
		if got != want {
			t.Errorf("%q: Auth = %d, want %d", header, got, want)
		}
	}
}

func TestRouter(t *testing.T) {
	var r Router
	mark := func(name string) Handler { return func(c *Ctx) { c.Scratch = append(c.Scratch[:0], name...) } }
	r.Handle(GET, "/", mark("root"))
	r.Handle(GET, "/user", mark("user"))
	r.Handle(GET, "/user/repos", mark("user-repos"))
	r.Handle(POST, "/user/repos", mark("create-repo"))
	r.Handle(GET, "/repos/{owner}/{repo}", mark("repo"))
	r.Handle(GET, "/repos/{owner}/{repo}/issues/{number}", mark("issue"))
	r.Handle(GET, "/repos/{owner}/{repo}/issues/comments/{id}", mark("comment"))
	r.Handle(GET, "/repos/{owner}/{repo}/contents", mark("contents-root"))
	r.Handle(GET, "/repos/{owner}/{repo}/contents/{path...}", mark("contents"))
	r.Handle(GET, "/repos/{owner}/{repo}/git/refs/heads/{branch...}", mark("head-ref"))
	r.Handle(GET, "/repos/{owner}/{repo}/git/refs/{ref...}", mark("any-ref"))
	r.Handle(PUT, "/repos/{owner}/{repo}/tail/{rest...}", mark("tail-only"))

	c := &Ctx{}
	for _, tc := range []struct {
		method Method
		path   string
		want   string
		params []string
	}{
		{GET, "", "root", nil},
		{GET, "/", "root", nil},
		{GET, "/user", "user", nil},
		{GET, "/user/", "user", nil},
		{GET, "/user/repos", "user-repos", nil},
		{POST, "/user/repos", "create-repo", nil},
		{DELETE, "/user/repos", "", nil},
		{GET, "/nope", "", nil},
		{GET, "/repos/ada/x", "repo", []string{"ada", "x"}},
		{GET, "/repos/ada", "", nil},
		{GET, "/repos/ada/x/issues/12", "issue", []string{"ada", "x", "12"}},
		{GET, "/repos/ada/x/issues/comments/7", "comment", []string{"ada", "x", "7"}},
		{GET, "/repos/ada/x/issues/comments", "issue", []string{"ada", "x", "comments"}},
		{GET, "/repos/ada/x/contents", "contents-root", []string{"ada", "x"}},
		{GET, "/repos/ada/x/contents/", "contents-root", []string{"ada", "x"}},
		{GET, "/repos/ada/x/contents/config/token.css", "contents", []string{"ada", "x", "config/token.css"}},
		{GET, "/repos/ada/x/git/refs/heads/feature/x", "head-ref", []string{"ada", "x", "feature/x"}},
		{GET, "/repos/ada/x/git/refs/tags/v1", "any-ref", []string{"ada", "x", "tags/v1"}},
		{GET, "/repos/ada/x/git/refs/heads", "any-ref", []string{"ada", "x", "heads"}},
		{PUT, "/repos/ada/x/tail", "", nil},
		{PUT, "/repos/ada/x/tail/a%2Fb", "tail-only", []string{"ada", "x", "a%2Fb"}},
	} {
		c.Scratch = c.Scratch[:0]
		h := r.Find(c, tc.method, []byte(tc.path))
		got := ""
		if h != nil {
			h(c)
			got = string(c.Scratch)
		}
		if got != tc.want {
			t.Errorf("%d %s -> %q, want %q", tc.method, tc.path, got, tc.want)
			continue
		}
		if h == nil {
			continue
		}
		if c.nparams != len(tc.params) {
			t.Errorf("%s: %d params, want %d", tc.path, c.nparams, len(tc.params))
			continue
		}
		for i, want := range tc.params {
			if string(c.Param(i)) != want {
				t.Errorf("%s: param %d = %q, want %q", tc.path, i, c.Param(i), want)
			}
		}
	}
	if s, ok := c.ParamString(2); !ok || s != "a/b" {
		t.Errorf("ParamString = %q %v", s, ok)
	}
	if c.Param(9) != nil || c.Param(-1) != nil {
		t.Error("out-of-range parameter must be nil")
	}
}

func TestQueryAndEscapes(t *testing.T) {
	c := &Ctx{Query: []byte("state=open&per_page=10&q=a+b%2Fc&flag&bad=%zz&page=x")}
	if v, ok := c.QueryRaw("state"); !ok || string(v) != "open" {
		t.Errorf("QueryRaw(state) = %q %v", v, ok)
	}
	if v, ok := c.QueryRaw("flag"); !ok || len(v) != 0 {
		t.Errorf("QueryRaw(flag) = %q %v", v, ok)
	}
	if _, ok := c.QueryRaw("missing"); ok {
		t.Error("QueryRaw(missing) found something")
	}
	if c.QueryString("q") != "a b/c" || c.QueryString("bad") != "" || c.QueryString("missing") != "" {
		t.Errorf("QueryString: %q %q", c.QueryString("q"), c.QueryString("bad"))
	}
	if c.QueryInt("per_page", 30) != 10 || c.QueryInt("page", 1) != 1 || c.QueryInt("missing", 7) != 7 {
		t.Error("QueryInt gives a wrong value")
	}
	if n, ok := ParseInt([]byte("999999999")); !ok || n != 999999999 {
		t.Error("ParseInt(999999999)")
	}
	for _, bad := range []string{"", "-1", "1x", "12345678901234567890", " 1"} {
		if _, ok := ParseInt([]byte(bad)); ok {
			t.Errorf("ParseInt(%q) accepted", bad)
		}
	}
	if out, ok := Unescape(nil, []byte("release%2F1.0%20x+y"), false); !ok || string(out) != "release/1.0 x+y" {
		t.Errorf("Unescape = %q %v", out, ok)
	}
	if _, ok := Unescape(nil, []byte("bad%2"), false); ok {
		t.Error("Unescape accepted a truncated escape")
	}
}

func TestHostileHostIsReplaced(t *testing.T) {
	_, c := start(t, echo)
	c.send("GET / HTTP/1.1\r\nHost: evil\"},{\"x\r\n\r\n")
	if res := c.read(true); !strings.Contains(res.body, `"host":"fallback:1"`) {
		t.Fatalf("host not replaced: %s", res.body)
	}
}

// The engine itself must not allocate for a request it has seen the like of before: parse, route,
// reply from cached bytes.
func BenchmarkEngineGet(b *testing.B) {
	var router Router
	tail := AppendTail(nil, bytes.Repeat([]byte("x"), 700))
	router.Handle(GET, "/repos/{owner}/{repo}/issues/{number}", func(c *Ctx) {
		if c.Auth() != AuthOK {
			c.JSON(401, []byte("{}"))
			return
		}
		if _, ok := c.ParamInt(2); !ok {
			c.JSON(404, []byte("{}"))
			return
		}
		c.JSONTail(200, tail)
	})
	srv := &Server{Token: "0123456789abcdef0123456789abcdef"}
	srv.Handler = func(c *Ctx) {
		if h := router.Find(c, c.Method, c.Path[len("/api/v3"):]); h != nil {
			h(c)
			return
		}
		c.JSON(404, []byte("{}"))
	}
	srv.init()
	c := srv.pool.Get().(*Ctx)
	req := []byte("GET /api/v3/repos/adaiversen/bench-read/issues/1 HTTP/1.1\r\nhost: 127.0.0.1:43210\r\nconnection: keep-alive\r\n" +
		"accept: application/vnd.github+json\r\nauthorization: Bearer 0123456789abcdef0123456789abcdef\r\nuser-agent: node\r\n" +
		"accept-language: *\r\nsec-fetch-mode: cors\r\naccept-encoding: gzip, deflate\r\n\r\n")
	b.ReportAllocs()
	b.SetBytes(int64(len(req)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		used, code := c.parse(req)
		if code != 0 || used != len(req) {
			b.Fatalf("parse: %d %d", used, code)
		}
		srv.dispatch(c)
		if len(c.out) < 700 {
			b.Fatal("no response")
		}
		c.out = c.out[:0]
		c.scanned, c.need = 0, 0
	}
}

// A large body costs its own size (not the next power of two), the buffer is given back once the
// request is answered, and nothing makes the buffer grow past the request size limit.
func TestBufferGrowthIsBounded(t *testing.T) {
	sizes := make(chan [2]int, 4)
	_, c := start(t, func(c *Ctx) {
		sizes <- [2]int{len(c.Body), len(c.in)}
		c.Empty(204)
	})
	const big = 3<<20 + 123
	go c.send("POST /big HTTP/1.1\r\nHost: h\r\nContent-Length: " + strconv.Itoa(big) + "\r\n\r\n" + strings.Repeat("b", big))
	c.read(true)
	if got := <-sizes; got[0] != big || got[1] < big || got[1] > big+maxHeaderSize {
		t.Fatalf("a %d byte body was read into a %d byte buffer", got[0], got[1])
	}
	c.send("POST /small HTTP/1.1\r\nHost: h\r\nContent-Length: 2\r\n\r\nok")
	c.read(true)
	if got := <-sizes; got[1] != inputSize {
		t.Fatalf("after a large request the buffer is still %d bytes", got[1])
	}

	// The decision itself, at the limit.
	ctx := &Ctx{in: make([]byte, inputSize)}
	ctx.end = len(ctx.in)
	if !ctx.room() || len(ctx.in) != 2*inputSize {
		t.Fatalf("a full small buffer must double, got %d", len(ctx.in))
	}
	ctx.in = make([]byte, maxRequestSize-4096)
	ctx.end = len(ctx.in)
	if !ctx.room() || len(ctx.in) != maxRequestSize {
		t.Fatalf("growth must stop at the request limit, got %d", len(ctx.in))
	}
	ctx.end = len(ctx.in)
	if ctx.room() {
		t.Fatal("a buffer full at the request limit must be refused")
	}
	ctx = &Ctx{in: make([]byte, inputSize), need: 5 << 20}
	if !ctx.room() || len(ctx.in) < 5<<20 || len(ctx.in) > 5<<20+4096 {
		t.Fatalf("an announced 5 MiB request got a %d byte buffer", len(ctx.in))
	}

	// Limits answered from the headers alone.
	for raw, want := range map[string]int{
		"POST / HTTP/1.1\r\nHost: h\r\nContent-Length: " + strconv.Itoa(maxBodySize+1) + "\r\n\r\n": 413,
		strings.Repeat("\r\n", 400) + "GET / HTTP/1.1\r\nHost: h\r\n\r\n":                           400,
	} {
		_, c := start(t, echo)
		c.send(raw)
		if res := c.read(true); res.status != want {
			t.Errorf("%q...: %d, want %d", raw[:20], res.status, want)
		}
		c.expectClosed()
	}
	// A couple of blank lines before a request are still fine.
	_, c = start(t, echo)
	c.send("\r\n\r\nGET /after-blank-lines HTTP/1.1\r\nHost: h\r\n\r\n")
	if res := c.read(true); res.status != 200 || !strings.Contains(res.body, "/after-blank-lines") {
		t.Fatalf("blank lines before a request: %d %s", res.status, res.body)
	}
}
