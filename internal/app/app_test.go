package app

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"forge/internal/httpd"
	"forge/internal/store"
)

type reply struct {
	status int
	header map[string]string
	body   []byte
}

// call sends one request through the whole forge (dispatcher, surface, engine) and parses the reply.
func call(t *testing.T, tester *httpd.Tester, method, path, token, body string) reply {
	t.Helper()
	req := method + " " + path + " HTTP/1.1\r\nHost: forge.test:4000\r\n"
	if token != "" {
		req += "Authorization: Bearer " + token + "\r\n"
	}
	if body != "" {
		req += "Content-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n"
	}
	raw := tester.Do([]byte(req + "\r\n" + body))
	head, payload, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		t.Fatalf("%s %s: malformed reply %q", method, path, raw)
	}
	lines := strings.Split(string(head), "\r\n")
	r := reply{header: map[string]string{}, body: bytes.Clone(payload)}
	r.status, _ = strconv.Atoi(lines[0][9:12])
	for _, line := range lines[1:] {
		name, value, _ := strings.Cut(line, ": ")
		r.header[strings.ToLower(name)] = value
	}
	if n, err := strconv.Atoi(r.header["content-length"]); err == nil && n != len(payload) {
		t.Fatalf("%s %s: Content-Length %d, body %d bytes", method, path, n, len(payload))
	}
	return r
}

func (r reply) message(t *testing.T) string {
	t.Helper()
	var m struct{ Message string }
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("body is not a JSON object: %s", r.body)
	}
	return m.Message
}

func TestDispatch(t *testing.T) {
	forge := New()
	tester := httpd.NewTester(&httpd.Server{Handler: forge.Serve, Token: "tok"})

	if r := call(t, tester, "GET", "/healthz", "", ""); r.status != 200 || string(r.body) != `{"ok":true}` {
		t.Fatalf("healthz: %d %s", r.status, r.body)
	}

	// REST: authentication comes first, for every route; then routing.
	for _, path := range []string{"/api/v3", "/api/v3/", "/api/v3/meta", "/api/v3/user", "/api/v3/no-such-route", "/api/v3/repos/adaiversen/x"} {
		r := call(t, tester, "GET", path, "", "")
		if r.status != 401 || r.message(t) != "Requires authentication" || r.header["x-github-request-id"] == "" || r.header["content-type"] != "application/json; charset=utf-8" {
			t.Errorf("anonymous %s: %d %s %v", path, r.status, r.body, r.header)
		}
		if r := call(t, tester, "GET", path, "wrong", ""); r.status != 401 || r.message(t) != "Bad credentials" {
			t.Errorf("wrong token %s: %d %s", path, r.status, r.body)
		}
	}
	for _, path := range []string{"/api/v3", "/api/v3/"} {
		r := call(t, tester, "GET", path, "tok", "")
		if r.status != 200 || r.header["x-oauth-scopes"] != "repo, read:org" || !bytes.Contains(r.body, []byte(`"current_user_url":"http://forge.test:4000/api/v3/user"`)) {
			t.Errorf("root %s: %d %v %s", path, r.status, r.header, r.body)
		}
	}
	if r := call(t, tester, "GET", "/api/v3/meta", "tok", ""); r.status != 200 || !bytes.Contains(r.body, []byte(`"installed_version":"3.14.0"`)) {
		t.Errorf("meta: %d %s", r.status, r.body)
	}
	if r := call(t, tester, "GET", "/api/v3/user", "tok", ""); r.status != 200 || !bytes.Contains(r.body, []byte(`"login":"adaiversen"`)) {
		t.Errorf("user: %d %s", r.status, r.body)
	}
	for _, path := range []string{"/api/v3/no-such-route/amber-thistle", "/api/v3x", "/nothing", "/", "/adaiversen/repo/info/refs"} {
		r := call(t, tester, "GET", path, "tok", "")
		if r.status != 404 || r.message(t) != "Not Found" || r.header["content-type"] != "application/json; charset=utf-8" {
			t.Errorf("unknown route %s: %d %s", path, r.status, r.body)
		}
	}
	if r := call(t, tester, "POST", "/api/v3/meta", "tok", "{}"); r.status != 404 {
		t.Errorf("a route without the method must be 404, got %d", r.status)
	}

	// GraphQL and MCP gates.
	if r := call(t, tester, "POST", "/api/graphql", "", `{"query":"{viewer{login}}"}`); r.status != 401 || !strings.Contains(r.message(t), "requires you to be authenticated") {
		t.Errorf("anonymous graphql: %d %s", r.status, r.body)
	}
	if r := call(t, tester, "POST", "/api/graphql", "tok", `{"query":"{viewer{login}}"}`); r.status != 200 {
		t.Errorf("graphql: %d %s", r.status, r.body)
	}
	if r := call(t, tester, "POST", "/mcp", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); r.status != 401 || r.header["www-authenticate"] != "Bearer" {
		t.Errorf("anonymous mcp: %d %v", r.status, r.header)
	}
	if r := call(t, tester, "GET", "/mcp", "tok", ""); r.status != 405 || r.header["allow"] != "POST" {
		t.Errorf("GET /mcp: %d %v", r.status, r.header)
	}

	// Git: a repository that does not exist, or another owner, is a 404.
	forge.Store.Lock()
	forge.Store.CreateRepo(store.NewRepo{Name: "real"})
	forge.Store.Unlock()
	for path, want := range map[string]int{
		"/adaiversen/missing.git/info/refs?service=git-upload-pack": 404,
		"/olgabauer/real.git/info/refs?service=git-upload-pack":     404,
		"/adaiversen/real.git/info/refs?service=git-upload-pack":    200,
	} {
		if r := call(t, tester, "GET", path, "tok", ""); r.status != want {
			t.Errorf("%s: %d, want %d", path, r.status, want)
		}
	}
}

func TestSplitGitPath(t *testing.T) {
	for path, want := range map[string]string{
		"/adaiversen/repo.git/info/refs":       "adaiversen|repo|/info/refs",
		"/adaiversen/repo.git/git-upload-pack": "adaiversen|repo|/git-upload-pack",
		"/adaiversen/re.po.git":                "adaiversen|re.po|",
		"/adaiversen/repo/info/refs":           "",
		"/adaiversen/.git/info/refs":           "",
		"/repo.git":                            "",
		"//repo.git/x":                         "",
		"/api/v3/repos":                        "",
	} {
		owner, repo, rest, ok := splitGitPath([]byte(path))
		got := ""
		if ok {
			got = string(owner) + "|" + string(repo) + "|" + string(rest)
		}
		if got != want {
			t.Errorf("splitGitPath(%q) = %q, want %q", path, got, want)
		}
	}
}
