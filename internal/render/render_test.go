package render

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"forge/internal/httpd"
	"forge/internal/store"
)

var host = []byte("127.0.0.1:4000")

const origin = "http://127.0.0.1:4000"

// object parses rendered bytes as a JSON object, failing the test when they are not valid JSON.
func object(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, b)
	}
	return m
}

func array(t *testing.T, b []byte) []any {
	t.Helper()
	var a []any
	if err := json.Unmarshal(b, &a); err != nil || a == nil {
		t.Fatalf("not a JSON array: %v\n%s", err, b)
	}
	return a
}

func TestAppendString(t *testing.T) {
	for _, s := range []string{"", "plain", `quote " and \ backslash`, "line\nbreak\ttab\rreturn", "ctl\x00\x01\x1f", "<b>&amp;</b>",
		"héllo 世界 \U0001F600", "sep arator", "bad\xff\xfeutf8", "ends with \\"} {
		got := AppendString(nil, s)
		var back string
		if err := json.Unmarshal(got, &back); err != nil {
			t.Errorf("AppendString(%q) = %s: %v", s, got, err)
			continue
		}
		if want := strings.ToValidUTF8(s, "�"); back != want && !strings.Contains(s, "\xff") {
			t.Errorf("AppendString(%q) round-trips to %q", s, back)
		}
	}
	if got := string(AppendString(nil, "<a href=\"x\">&</a>")); got != `"<a href=\"x\">&</a>"` {
		t.Errorf("HTML characters must not be escaped: %s", got)
	}
}

func TestAppendTime(t *testing.T) {
	for _, ts := range []int64{0, 1, 59, 86399, 86400, 951782400, 951868799, 1709164800, 1767225600, 1767225601, 1798761599, 4102444800, 13569465600} {
		want := time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
		if got := string(AppendTime(nil, ts)); got != want {
			t.Errorf("AppendTime(%d) = %s, want %s", ts, got, want)
		}
	}
	// Every day for a few years around the epoch the clock starts at, leap day included.
	for ts := int64(store.Epoch) - 800*86400; ts < store.Epoch+1200*86400; ts += 86400 - 7 {
		want := time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
		if got := string(AppendTime(nil, ts)); got != want {
			t.Fatalf("AppendTime(%d) = %s, want %s", ts, got, want)
		}
	}
}

func fixture(t *testing.T) (*store.Store, *store.Repo) {
	t.Helper()
	s := store.New()
	r, err := s.CreateRepo(store.NewRepo{Name: "amber-thistle", Private: true, Description: "the tooling behind the thing", HasDescription: true})
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}

func TestRepoAndUser(t *testing.T) {
	_, r := fixture(t)
	m := object(t, AppendRepo(nil, host, r))
	for key, want := range map[string]any{
		"id": float64(1), "node_id": "R_UmVwb3NpdG9yeTox", "name": "amber-thistle", "full_name": "adaiversen/amber-thistle",
		"private": true, "visibility": "private", "description": "the tooling behind the thing", "default_branch": "main",
		"html_url": origin + "/adaiversen/amber-thistle", "url": origin + "/api/v3/repos/adaiversen/amber-thistle",
		"clone_url": origin + "/adaiversen/amber-thistle.git", "has_issues": true, "fork": false, "archived": false,
		"open_issues_count": float64(0), "created_at": "2026-01-01T00:00:01Z",
	} {
		if m[key] != want {
			t.Errorf("repo.%s = %v, want %v", key, m[key], want)
		}
	}
	owner := m["owner"].(map[string]any)
	if owner["login"] != "adaiversen" || owner["id"] != float64(1000) || owner["node_id"] != "U_VXNlcjoxMDAw" {
		t.Errorf("owner = %v", owner)
	}
	r.HasDescription = false
	if m := object(t, AppendRepo(nil, host, r)); m["description"] != nil {
		t.Errorf("an unset description must be null, got %v", m["description"])
	}
	if _, has := object(t, AppendRepo(nil, host, r))["description"]; !has {
		t.Error("the description key must always be present")
	}
	u := object(t, AppendViewer(nil, host))
	if u["login"] != "adaiversen" || u["name"] != "ada iversen" || u["email"] != "adaiversen@example.invalid" || u["url"] != origin+"/api/v3/user" {
		t.Errorf("viewer = %v", u)
	}
}

func TestIssueRenderingAndCache(t *testing.T) {
	s, r := fixture(t)
	r.CreateLabel("bug", "ededed", "", false)
	is, _ := r.CreateIssue(store.NewIssue{Title: "Incorrect \"GraphQL\" node ids", Body: "line one\nline <two> & three", Labels: []string{"bug"}})
	body := AppendIssue(nil, host, is)
	m := object(t, body)
	for key, want := range map[string]any{
		"id": float64(1), "node_id": "I_SXNzdWU6MQ", "number": float64(1), "title": "Incorrect \"GraphQL\" node ids",
		"body": "line one\nline <two> & three", "state": "open", "state_reason": nil, "comments": float64(0), "closed_at": nil,
		"url": origin + "/api/v3/repos/adaiversen/amber-thistle/issues/1", "html_url": origin + "/adaiversen/amber-thistle/issues/1",
	} {
		if m[key] != want {
			t.Errorf("issue.%s = %v, want %v", key, m[key], want)
		}
	}
	labels := m["labels"].([]any)
	if len(labels) != 1 || labels[0].(map[string]any)["name"] != "bug" {
		t.Errorf("labels = %v", labels)
	}
	if a, ok := m["assignees"].([]any); !ok || len(a) != 0 {
		t.Errorf("assignees must be an empty array, got %v", m["assignees"])
	}

	// The tail is rendered once and reused until the issue changes.
	tail := IssueTail(host, is)
	if &tail[0] != &IssueTail(host, is)[0] {
		t.Fatal("the tail was rendered twice")
	}
	if want := "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + string(body); string(tail) != want {
		t.Fatalf("tail = %q", tail)
	}
	// Another host (another origin in the URLs) gets its own rendering.
	if other := AppendIssue(nil, []byte("forge.test"), is); !bytes.Contains(other, []byte(`"url":"http://forge.test/api/v3/`)) {
		t.Fatalf("issue for another host: %s", other)
	}
	// Each kind of change shows on the next read.
	closed := "closed"
	r.UpdateIssue(is, store.IssuePatch{State: &closed})
	if m := object(t, AppendIssue(nil, host, is)); m["state"] != "closed" || m["state_reason"] != "completed" || m["closed_at"] == nil {
		t.Fatalf("after close: %v", m)
	}
	r.AddComment(is.Number, "a comment")
	if m := object(t, AppendIssue(nil, host, is)); m["comments"] != float64(1) {
		t.Fatalf("after a comment: %v", m["comments"])
	}
	name := "renamed"
	s.UpdateRepo(r, store.RepoPatch{Name: &name})
	if m := object(t, AppendIssue(nil, host, is)); m["html_url"] != origin+"/adaiversen/renamed/issues/1" {
		t.Fatalf("after a repository rename: %v", m["html_url"])
	}

	// An issue with no body and no labels still has both keys with the right types.
	plain, _ := r.CreateIssue(store.NewIssue{Title: "plain"})
	m = object(t, AppendIssue(nil, host, plain))
	if b, ok := m["body"].(string); !ok || b != "" {
		t.Errorf("body must be an empty string, got %v", m["body"])
	}
	if l, ok := m["labels"].([]any); !ok || len(l) != 0 {
		t.Errorf("labels must be an empty array, got %v", m["labels"])
	}
	if list := array(t, AppendIssues(nil, host, r.Issues)); len(list) != 2 {
		t.Errorf("issue list has %d items", len(list))
	}
	if string(AppendIssues(nil, host, nil)) != "[]" || string(AppendComments(nil, host, nil)) != "[]" || string(AppendLabels(nil, host, nil)) != "[]" || string(AppendPulls(nil, host, nil)) != "[]" {
		t.Error("empty lists must render as []")
	}
	c, _ := r.Comments(is.Number)
	k := object(t, AppendComment(nil, host, c[0]))
	if k["id"] != float64(1) || k["body"] != "a comment" || k["node_id"] != "IC_SXNzdWVDb21tZW50OjE" ||
		k["html_url"] != origin+"/adaiversen/renamed/issues/1#issuecomment-1" {
		t.Errorf("comment = %v", k)
	}
	l := object(t, AppendLabel(nil, host, r.Labels[0]))
	if l["name"] != "bug" || l["color"] != "ededed" || l["description"] != nil || l["default"] != false {
		t.Errorf("label = %v", l)
	}
}

func TestGitObjectsAndPulls(t *testing.T) {
	_, r := fixture(t)
	content := []byte("nickel\ncharlie\ngranite\n")
	first, err := r.PutFile(store.FileWrite{Path: "config/token.css", Message: "Tidy issue pagination", Data: content})
	if err != nil {
		t.Fatal(err)
	}
	put := object(t, AppendFileResult(nil, host, r, &first, false))
	pc, commit := put["content"].(map[string]any), put["commit"].(map[string]any)
	if pc["sha"] != first.Blob.String() || pc["encoding"] != "base64" || pc["path"] != "config/token.css" || pc["name"] != "token.css" || pc["size"] != float64(len(content)) {
		t.Errorf("put content = %v", pc)
	}
	if commit["sha"] != first.Commit.ID.String() || commit["message"] != "Tidy issue pagination" {
		t.Errorf("put commit = %v", commit)
	}
	del := object(t, AppendFileResult(nil, host, r, &first, true))
	if v, has := del["content"]; !has || v != nil {
		t.Errorf("delete reply content = %v", v)
	}

	file, _ := r.Contents("config/token.css", "")
	f := object(t, AppendContent(nil, host, r, "config/token.css", &file))
	raw, err := base64.StdEncoding.DecodeString(f["content"].(string))
	if err != nil || !bytes.Equal(raw, content) || f["type"] != "file" || f["encoding"] != "base64" || f["sha"] != first.Blob.String() || f["size"] != float64(len(content)) {
		t.Errorf("file = %v (%v)", f, err)
	}
	dir, _ := r.Contents("", "")
	entries := array(t, AppendContent(nil, host, r, "", &dir))
	if e := entries[0].(map[string]any); len(entries) != 1 || e["type"] != "dir" || e["name"] != "config" || e["path"] != "config" || e["size"] != float64(0) {
		t.Errorf("root listing = %v", entries)
	}
	sub, _ := r.Contents("config", "")
	if e := array(t, AppendContent(nil, host, r, "config", &sub))[0].(map[string]any); e["type"] != "file" || e["path"] != "config/token.css" || e["size"] != float64(len(content)) {
		t.Errorf("directory listing = %v", e)
	}

	branch := r.Branches()[0]
	b := object(t, AppendBranch(nil, host, r, branch))
	if b["name"] != "main" || b["commit"].(map[string]any)["sha"] != first.Commit.ID.String() {
		t.Errorf("branch = %v", b)
	}
	g := object(t, AppendGitRef(nil, host, r, branch))
	if g["ref"] != "refs/heads/main" || g["object"].(map[string]any)["sha"] != first.Commit.ID.String() || g["object"].(map[string]any)["type"] != "commit" ||
		g["url"] != origin+"/api/v3/repos/adaiversen/amber-thistle/git/refs/heads/main" {
		t.Errorf("git ref = %v", g)
	}
	c := object(t, AppendCommit(nil, host, r, first.Commit))
	inner := c["commit"].(map[string]any)
	if c["sha"] != first.Commit.ID.String() || inner["message"] != "Tidy issue pagination" || len(c["parents"].([]any)) != 0 ||
		inner["author"].(map[string]any)["email"] != "adaiversen@example.invalid" || c["author"].(map[string]any)["login"] != "adaiversen" {
		t.Errorf("commit = %v", c)
	}

	// A pull request, open then merged.
	r.CreateRef("refs/heads/chore/mcp-tool-schemas", first.Commit.ID)
	second, _ := r.PutFile(store.FileWrite{Path: "config/new file.css", Message: "add", Data: []byte("a\nb\n"), Branch: "chore/mcp-tool-schemas"})
	p, err := r.CreatePull(store.NewPull{Title: "Improve MCP tool schemas", Head: "chore/mcp-tool-schemas", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	m := object(t, AppendPull(nil, host, p))
	head, base := m["head"].(map[string]any), m["base"].(map[string]any)
	if m["number"] != float64(1) || m["state"] != "open" || m["body"] != "" || m["merged"] != false || m["mergeable"] != true || m["merged_at"] != nil ||
		m["merge_commit_sha"] != nil || m["commits"] != float64(1) || m["additions"] != float64(2) || m["changed_files"] != float64(1) ||
		head["ref"] != "chore/mcp-tool-schemas" || head["sha"] != second.Commit.ID.String() || head["label"] != "adaiversen:chore/mcp-tool-schemas" ||
		base["ref"] != "main" || base["sha"] != first.Commit.ID.String() || base["repo"].(map[string]any)["name"] != "amber-thistle" ||
		m["html_url"] != origin+"/adaiversen/amber-thistle/pull/1" {
		t.Errorf("open pull = %v", m)
	}
	info := r.PullInfo(p)
	files := array(t, AppendPullFiles(nil, host, r, info, info.Files))
	if f := files[0].(map[string]any); len(files) != 1 || f["filename"] != "config/new file.css" || f["status"] != "added" || f["additions"] != float64(2) ||
		f["changes"] != float64(2) || !strings.Contains(f["contents_url"].(string), "/contents/config/new%20file.css?ref=") {
		t.Errorf("pull files = %v", files)
	}
	merge, err := r.MergePull(p, store.MergeOptions{Method: "squash"})
	if err != nil {
		t.Fatal(err)
	}
	res := object(t, AppendMergeResult(nil, merge.ID))
	if res["merged"] != true || res["sha"] != merge.ID.String() || res["message"] != "Pull Request successfully merged" {
		t.Errorf("merge result = %v", res)
	}
	m = object(t, AppendPull(nil, host, p))
	if m["state"] != "closed" || m["merged"] != true || m["merge_commit_sha"] != merge.ID.String() || m["merged_at"] == nil || m["mergeable"] != nil ||
		m["base"].(map[string]any)["sha"] != first.Commit.ID.String() || m["merged_by"].(map[string]any)["login"] != "adaiversen" {
		t.Errorf("merged pull = %v", m)
	}
}

func TestErrors(t *testing.T) {
	s, r := fixture(t)
	_, err := s.CreateRepo(store.NewRepo{Name: "amber-thistle"})
	body := AppendError(nil, err.(*store.Error))
	m := object(t, body)
	if m["message"] != "Repository creation failed." || !strings.Contains(string(body[:min(len(body), 400)]), "already exists") ||
		m["documentation_url"] != "https://docs.github.com/rest/repos/repos#create-a-repository-for-the-authenticated-user" {
		t.Errorf("duplicate repository error = %s", body)
	}
	_, err = r.CreateIssue(store.NewIssue{Title: "t", Labels: []string{"bug-not-a-label"}})
	want := `{"message":"Validation Failed","errors":[{"resource":"Label","field":"labels","code":"invalid","value":"bug-not-a-label"}],"documentation_url":"https://docs.github.com/rest"}`
	if got := string(AppendError(nil, err.(*store.Error))); got != want {
		t.Errorf("unknown label error = %s", got)
	}
	if got := string(AppendError(nil, store.ErrNotFound)); got != `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}` || got != string(AppendMessage(nil, "Not Found")) {
		t.Errorf("not found error = %s", got)
	}
}

// issueServer is a forge reduced to the two timed REST routes, written the way the REST surface
// writes them: look up, call the store, reply from the renderer.
func issueServer(st *store.Store) *httpd.Server {
	var router httpd.Router
	router.Handle(httpd.GET, "/api/v3/repos/{owner}/{repo}/issues/{number}", func(c *httpd.Ctx) {
		r := st.Lookup(c.Param(0), c.Param(1))
		n, ok := c.ParamInt(2)
		if r == nil || !ok || r.Issue(n) == nil {
			c.JSON(404, []byte(`{"message":"Not Found"}`))
			return
		}
		c.JSONTail(200, IssueTail(c.Host, r.Issue(n)))
	})
	router.Handle(httpd.POST, "/api/v3/repos/{owner}/{repo}/issues", func(c *httpd.Ctx) {
		r := st.Lookup(c.Param(0), c.Param(1))
		var in struct{ Title, Body string }
		if r == nil || json.Unmarshal(c.Body, &in) != nil {
			c.JSON(400, []byte(`{"message":"Problems parsing JSON"}`))
			return
		}
		is, err := r.CreateIssue(store.NewIssue{Title: in.Title, Body: in.Body})
		if err != nil {
			c.JSON(422, AppendError(nil, err.(*store.Error)))
			return
		}
		c.JSONTail(201, IssueTail(c.Host, is))
	})
	return &httpd.Server{Token: "0123456789abcdef", Handler: func(c *httpd.Ctx) {
		if c.Auth() != httpd.AuthOK {
			c.JSON(401, []byte(`{"message":"Requires authentication"}`))
			return
		}
		st.Lock()
		defer st.Unlock()
		if h := router.Find(c, c.Method, c.Path); h != nil {
			h(c)
			return
		}
		c.JSON(404, []byte(`{"message":"Not Found"}`))
	}}
}

const requestTail = "host: 127.0.0.1:43210\r\nconnection: keep-alive\r\naccept: application/vnd.github+json\r\n" +
	"authorization: Bearer 0123456789abcdef\r\nuser-agent: node\r\naccept-language: *\r\nsec-fetch-mode: cors\r\naccept-encoding: gzip, deflate\r\n"

const benchBody = "A minimal case is attached but the second request fails. The same flow works over the API and the server answers with a JSON error. Steps to reproduce are below but the second request fails."

// The read workload's request, end to end through the engine: it must not allocate.
func BenchmarkGetIssue(b *testing.B) {
	st := store.New()
	r, _ := st.CreateRepo(store.NewRepo{Name: "bench-read", Private: true})
	r.CreateIssue(store.NewIssue{Title: "Incorrect GraphQL node ids (bench setup)", Body: benchBody})
	tester := httpd.NewTester(issueServer(st))
	req := []byte("GET /api/v3/repos/adaiversen/bench-read/issues/1 HTTP/1.1\r\n" + requestTail + "\r\n")
	if res := tester.Do(req); !bytes.HasPrefix(res, []byte("HTTP/1.1 200 OK\r\n")) {
		b.Fatalf("unexpected reply: %s", res)
	} else {
		b.Logf("response: %d bytes, body %d bytes", len(res), len(res)-bytes.Index(res, []byte("\r\n\r\n"))-4)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if res := tester.Do(req); len(res) < 200 {
			b.Fatal("short reply")
		}
	}
}

// The write workload's request. What it allocates is what it stores: the decoded strings, the issue
// and its rendered reply.
func BenchmarkPostIssue(b *testing.B) {
	st := store.New()
	st.CreateRepo(store.NewRepo{Name: "bench-write", Private: true})
	tester := httpd.NewTester(issueServer(st))
	body := `{"title":"Incorrect GraphQL node ids [bench 1]","body":"` + benchBody + `"}`
	req := []byte("POST /api/v3/repos/adaiversen/bench-write/issues HTTP/1.1\r\n" + requestTail +
		"content-type: application/json\r\ncontent-length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body)
	if res := tester.Do(req); !bytes.HasPrefix(res, []byte("HTTP/1.1 201 Created\r\n")) {
		b.Fatalf("unexpected reply: %s", res)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if res := tester.Do(req); len(res) < 200 {
			b.Fatal("short reply")
		}
	}
}
