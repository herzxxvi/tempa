package main

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseEndpoint(t *testing.T) {
	for raw, want := range map[string]endpoint{
		"http://store:8080":           {addr: "store:8080", host: "store:8080"},
		"http://store:8080/":          {addr: "store:8080", host: "store:8080"},
		" http://127.0.0.1:41234// ":  {addr: "127.0.0.1:41234", host: "127.0.0.1:41234"},
		"http://store":                {addr: "store:80", host: "store"},
		"http://[::1]:9000":           {addr: "[::1]:9000", host: "[::1]:9000"},
		"http://[::1]":                {addr: "[::1]:80", host: "[::1]"},
		"http://store:8080/base/path": {addr: "store:8080", host: "store:8080", base: "/base/path"},
	} {
		if got, err := parseEndpoint(raw); err != nil || got != want {
			t.Errorf("parseEndpoint(%q) = %+v, %v; want %+v", raw, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", "https://store:8080", "store:8080", "http://"} {
		if _, err := parseEndpoint(bad); err == nil {
			t.Errorf("parseEndpoint(%q) accepted", bad)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	want := []time.Duration{250, 500, 1000, 2000, 2000, 2000}
	for attempt, ms := range want {
		if got := retryDelay(attempt); got != ms*time.Millisecond {
			t.Errorf("retryDelay(%d) = %v", attempt, got)
		}
	}
	if retryDelay(1000) != 2*time.Second {
		t.Error("retryDelay must stay capped")
	}
}

// fakeEnv is a minimal stand-in for the grading environment: it answers /register and /done the
// way a Node server does, in the framing chosen by the test.
type fakeEnv struct {
	ln       net.Listener
	chunked  bool
	requests atomic.Int32
	port     atomic.Int32 // the port the forge registered
	doneAt   int32        // /done answers true from this many polls on
	polls    atomic.Int32
	failures atomic.Int32 // the first N register calls answer 503
}

func startEnv(t *testing.T, chunked bool) *fakeEnv {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &fakeEnv{ln: ln, chunked: chunked, doneAt: 2}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go e.serve(conn)
		}
	}()
	return e
}

func (e *fakeEnv) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		return
	}
	length := 0
	for {
		h, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if h == "\r\n" {
			break
		}
		if name, value, _ := strings.Cut(h, ":"); strings.EqualFold(name, "content-length") {
			length, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}
	body := make([]byte, length)
	io.ReadFull(r, body)
	e.requests.Add(1)

	status, answer := "404 Not Found", `{"error":"not_found"}`
	switch {
	case strings.HasPrefix(line, "POST /register "):
		if e.failures.Add(-1) >= 0 {
			status, answer = "503 Service Unavailable", `{"ok":false}`
			break
		}
		port, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(string(body), `{"port":`), "}"))
		e.port.Store(int32(port))
		status, answer = "200 OK", `{"ok":true}`
	case strings.HasPrefix(line, "GET /done "):
		status, answer = "200 OK", `{"done":false}`
		if e.polls.Add(1) >= e.doneAt {
			answer = `{"done":true}`
		}
	}
	head := "HTTP/1.1 " + status + "\r\nContent-Type: application/json\r\nConnection: close\r\n"
	if e.chunked {
		half := len(answer) / 2
		io.WriteString(conn, head+"Transfer-Encoding: chunked\r\n\r\n"+
			strconv.FormatInt(int64(half), 16)+"\r\n"+answer[:half]+"\r\n"+
			strconv.FormatInt(int64(len(answer)-half), 16)+"\r\n"+answer[half:]+"\r\n0\r\n\r\n")
		return
	}
	io.WriteString(conn, head+"Content-Length: "+strconv.Itoa(len(answer))+"\r\n\r\n"+answer)
}

func TestRegisterAndDone(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		env := startEnv(t, chunked)
		env.failures.Store(1) // the first register is refused and must be retried
		e, err := parseEndpoint("http://" + env.ln.Addr().String() + "/")
		if err != nil {
			t.Fatal(err)
		}
		env0 := &environment{endpoints: []endpoint{e}}
		if err := register(env0, 43210); err != nil {
			t.Fatalf("register (chunked=%v): %v", chunked, err)
		}
		if env.port.Load() != 43210 {
			t.Fatalf("the environment saw port %d", env.port.Load())
		}
		finished := make(chan struct{})
		go func() { waitDone(env0); close(finished) }()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatalf("waitDone did not return (chunked=%v)", chunked)
		}
		if env.polls.Load() != 2 {
			t.Fatalf("polled %d times, want 2", env.polls.Load())
		}
	}
}

func TestRegisterContractError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 4096)
			conn.Read(buf)
			io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 20\r\n\r\n{\"error\":\"bad_port\"}")
			conn.Close()
		}
	}()
	e, _ := parseEndpoint("http://" + ln.Addr().String())
	if err := register(&environment{endpoints: []endpoint{e}}, 1); err == nil || !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "bad_port") {
		t.Fatalf("register against a 400: %v", err)
	}
}

func TestParseResponse(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		eof    bool
		status int
		body   string
		ok     bool
	}{
		{"HTTP/1.1 200 OK\r\nContent-Length: 13\r\n\r\n{\"done\":true}", false, 200, `{"done":true}`, true},
		{"HTTP/1.1 200 OK\r\ncontent-length: 13\r\n\r\n{\"done\":tr", false, 0, "", false},
		{"HTTP/1.1 200 OK\r\nContent-Le", false, 0, "", false},
		{"HTTP/1.1 409 Conflict\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n", false, 409, "hello", true},
		{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhel", false, 0, "", false},
		{"HTTP/1.0 200 OK\r\n\r\nuntil close", false, 0, "", false},
		{"HTTP/1.0 200 OK\r\n\r\nuntil close", true, 200, "until close", true},
		{"garbage\r\n\r\n", true, 0, "", false},
	} {
		status, body, ok := parseResponse([]byte(tc.raw), tc.eof)
		if ok != tc.ok || (ok && (status != tc.status || string(body) != tc.body)) {
			t.Errorf("parseResponse(%q, %v) = %d %q %v", tc.raw, tc.eof, status, body, ok)
		}
	}
}

// The launcher's pre-resolved URL is tried first; when it leads nowhere the original URL is used,
// and stays in use.
func TestEnvironmentFallback(t *testing.T) {
	env := startEnv(t, false)
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + dead.Addr().String()
	dead.Close() // nothing listens there any more
	liveURL := "http://" + env.ln.Addr().String()

	e, err := newEnvironment(liveURL, deadURL)
	if err != nil || len(e.endpoints) != 2 || e.endpoints[0].addr != dead.Addr().String() || e.endpoints[0].host != e.endpoints[1].host {
		t.Fatalf("newEnvironment: %+v %v", e, err)
	}
	if err := register(e, 4321); err != nil || env.port.Load() != 4321 || e.current != 1 {
		t.Fatalf("register through the fallback: %v, port %d, current %d", err, env.port.Load(), e.current)
	}
	before := env.requests.Load()
	if status, _, err := e.do("GET", "/done", nil); err != nil || status != 200 || env.requests.Load() != before+1 {
		t.Fatalf("poll after the fallback: %d %v", status, err)
	}
	// The usual cases: no pre-resolved URL, the same URL twice, a broken one.
	for _, resolved := range []string{"", liveURL, liveURL + "/", "not a url"} {
		if e, err := newEnvironment(liveURL, resolved); err != nil || len(e.endpoints) != 1 {
			t.Errorf("newEnvironment(%q, %q): %+v %v", liveURL, resolved, e, err)
		}
	}
	if _, err := newEnvironment("", liveURL); err == nil {
		t.Error("a missing MAKEFUN_ENV_URL must stay an error")
	}
}

// The self-check gives up within its budget whether the port refuses, stalls or answers wrongly.
func TestProbeHealthIsBounded(t *testing.T) {
	// A listener that accepts and then says nothing: the probe must not wait for it.
	stalled, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	go func() {
		for {
			conn, err := stalled.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	refused, _ := net.Listen("tcp", "127.0.0.1:0")
	refusedAddr := refused.Addr().String()
	refused.Close()
	for name, addr := range map[string]string{"stalled": stalled.Addr().String(), "refused": refusedAddr} {
		begin := time.Now()
		if probeHealth(addr, 600*time.Millisecond) {
			t.Errorf("%s: probe reported healthy", name)
		}
		if took := time.Since(begin); took < 550*time.Millisecond || took > 1200*time.Millisecond {
			t.Errorf("%s: probe took %v with a 600 ms budget", name, took)
		}
	}
	env := startEnv(t, false) // answers 404 to /healthz: reachable, but not healthy
	if probeHealth(env.ln.Addr().String(), 300*time.Millisecond) {
		t.Error("a 404 counted as healthy")
	}
}
