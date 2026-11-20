package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"forge/internal/httpd"
)

// The environment speaks plain HTTP/1.1 on a private network. A register call and one poll every
// two seconds do not justify linking net/http, so this file carries the few lines of client needed:
// one request per connection, "Connection: close", the response read to its end.

// endpoint is where the environment listens.
type endpoint struct {
	addr string // host:port to dial
	host string // Host header
	base string // path prefix of the URL, without a trailing slash; usually empty
}

// parseEndpoint reads MAKEFUN_ENV_URL, e.g. "http://store:8080".
func parseEndpoint(raw string) (endpoint, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return endpoint{}, errors.New("not set: there is no environment to register with")
	}
	rest, ok := strings.CutPrefix(raw, "http://")
	if !ok {
		return endpoint{}, errors.New("must be an http:// URL, got " + raw)
	}
	authority, path, _ := strings.Cut(rest, "/")
	if authority == "" {
		return endpoint{}, errors.New("has no host: " + raw)
	}
	e := endpoint{addr: authority, host: authority}
	if path != "" {
		e.base = "/" + path
	}
	if _, _, err := net.SplitHostPort(authority); err != nil {
		e.addr = net.JoinHostPort(strings.Trim(authority, "[]"), "80")
	}
	return e, nil
}

// environment is the grading environment as this process can reach it: one or two URLs for the same
// server. When the launcher resolved the environment's host name with the system resolver it passes
// the resulting URL in FORGE_ENV_URL, and that one is tried first; MAKEFUN_ENV_URL itself, resolved
// by Go's own resolver, is the other. Whichever answered last is used first the next time.
type environment struct {
	endpoints []endpoint
	current   int
}

// newEnvironment builds the environment from the two variables. MAKEFUN_ENV_URL must be valid;
// a missing or unusable FORGE_ENV_URL is simply not used.
func newEnvironment(envURL, resolvedURL string) (*environment, error) {
	original, err := parseEndpoint(envURL)
	if err != nil {
		return nil, err
	}
	env := &environment{}
	if resolved, err := parseEndpoint(resolvedURL); err == nil && resolved != original {
		// Same server, so the same Host header as the original URL would send.
		resolved.host = original.host
		env.endpoints = append(env.endpoints, resolved)
	}
	env.endpoints = append(env.endpoints, original)
	return env, nil
}

// do performs one exchange with the environment, moving on to the other endpoint when one cannot
// be reached at all. An HTTP answer of any status counts as reached.
func (env *environment) do(method, path string, body []byte) (status int, response []byte, err error) {
	for range env.endpoints {
		status, response, err = request(env.endpoints[env.current], method, path, body, requestTimeout)
		if err == nil {
			return status, response, nil
		}
		env.current = (env.current + 1) % len(env.endpoints)
	}
	return 0, nil, err
}

const (
	requestTimeout = 30 * time.Second
	dialTimeout    = 3 * time.Second
	retryInitial   = 250 * time.Millisecond
	retryMax       = 2 * time.Second
	maxResponse    = 1 << 20
)

// retryDelay is the wait after attempt failed or unfinished requests: 250 ms doubling to 2 s.
func retryDelay(attempt int) time.Duration {
	return min(retryMax, retryInitial<<min(attempt, 8))
}

// request performs one HTTP exchange and returns the status and the decoded body.
func request(e endpoint, method, path string, body []byte, timeout time.Duration) (status int, response []byte, err error) {
	conn, err := net.DialTimeout("tcp", e.addr, min(timeout, dialTimeout))
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	req := make([]byte, 0, 256+len(body))
	req = append(req, method...)
	req = append(req, ' ')
	req = append(req, e.base...)
	req = append(req, path...)
	req = append(req, " HTTP/1.1\r\nHost: "...)
	req = append(req, e.host...)
	req = append(req, "\r\nAccept: application/json\r\nConnection: close\r\n"...)
	if body != nil {
		req = append(req, "Content-Type: application/json\r\nContent-Length: "...)
		req = strconv.AppendInt(req, int64(len(body)), 10)
		req = append(req, "\r\n"...)
	}
	req = append(req, "\r\n"...)
	req = append(req, body...)
	if _, err := conn.Write(req); err != nil {
		return 0, nil, err
	}

	buf := make([]byte, 0, 4096)
	for {
		if len(buf) == cap(buf) {
			if len(buf) >= maxResponse {
				return 0, nil, errors.New("response too large")
			}
			buf = append(buf, 0)[:len(buf)]
		}
		n, rerr := conn.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if status, response, ok := parseResponse(buf, rerr != nil); ok {
			return status, response, nil
		}
		if rerr != nil {
			return 0, nil, errors.New("incomplete response: " + rerr.Error())
		}
	}
}

// parseResponse decodes a response once all of it has arrived. eof says the peer closed, which
// ends a body that has neither a length nor chunked framing.
func parseResponse(buf []byte, eof bool) (status int, body []byte, ok bool) {
	headEnd := bytes.Index(buf, []byte("\r\n\r\n"))
	if headEnd < 0 || len(buf) < 12 || !bytes.HasPrefix(buf, []byte("HTTP/1.")) {
		return 0, nil, false
	}
	status, err := strconv.Atoi(string(buf[9:12]))
	if err != nil {
		return 0, nil, false
	}
	head := bytes.ToLower(buf[:headEnd])
	rest := buf[headEnd+4:]
	if bytes.Contains(head, []byte("\r\ntransfer-encoding:")) && bytes.Contains(head, []byte("chunked")) {
		body, ok := httpd.DecodeChunked(rest)
		return status, body, ok
	}
	if i := bytes.Index(head, []byte("\r\ncontent-length:")); i >= 0 {
		value := head[i+len("\r\ncontent-length:"):]
		if j := bytes.IndexByte(value, '\r'); j >= 0 {
			value = value[:j]
		}
		n, err := strconv.Atoi(string(bytes.TrimSpace(value)))
		if err != nil || n < 0 {
			return 0, nil, false
		}
		if len(rest) < n {
			return 0, nil, false
		}
		return status, rest[:n], true
	}
	return status, rest, eof
}

// waitHealthy probes our own /healthz over loopback, so the environment's two probes right after
// register cannot arrive before the server answers.
func waitHealthy(port int) bool {
	self := endpoint{addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), host: "127.0.0.1"}
	for attempt := 0; attempt < 40; attempt++ {
		if status, _, err := request(self, "GET", "/healthz", nil, 2*time.Second); err == nil && status == 200 {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// register announces the port until the environment accepts it. 200 is the normal answer; 409 means
// a register already won, which is fine; 400 and 405 are contract errors and fatal. Network errors
// and other statuses are retried: the environment may still be starting.
func register(env *environment, port int) error {
	body := []byte(`{"port":` + strconv.Itoa(port) + `}`)
	for attempt := 0; ; attempt++ {
		status, response, err := env.do("POST", "/register", body)
		if err == nil {
			switch status {
			case 200, 409:
				return nil
			case 400, 405:
				return errors.New("register answered HTTP " + strconv.Itoa(status) + ": " + string(response))
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

// waitDone polls /done until the environment says the trial is over. Any error or any other answer
// means "not done yet".
func waitDone(env *environment) {
	for attempt := 0; ; attempt++ {
		status, response, err := env.do("GET", "/done", nil)
		if err == nil && status == 200 {
			var answer struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(response, &answer) == nil && answer.Done {
				return
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}
