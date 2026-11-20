package rest

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"forge/internal/httpd"
	"forge/internal/store"
)

// The tests drive the surface through the real engine (parser, router, response writer), mounted
// the way internal/app mounts it: under /api/v3 with the store locked around every request.

const (
	token  = "0123456789abcdef"
	origin = "http://forge.test:4000"
	api    = "/api/v3"
)

type client struct {
	t      testing.TB
	st     *store.Store
	tester *httpd.Tester
}

func newClient(t testing.TB) *client {
	st := store.New()
	surface := New(st)
	srv := &httpd.Server{Token: token, Handler: func(c *httpd.Ctx) {
		if !bytes.HasPrefix(c.Path, []byte(api)) {
			c.JSON(404, []byte(`{"message":"Not Found"}`))
			return
		}
		st.Lock()
		defer st.Unlock()
		surface.Serve(c, c.Path[len(api):])
	}}
	return &client{t: t, st: st, tester: httpd.NewTester(srv)}
}

type reply struct {
	t      testing.TB
	what   string
	status int
	header map[string]string
	body   []byte
}

// send issues one request. auth is the complete Authorization value, "" for none; body may be a
// string (sent as is) or any value (sent as JSON), nil for none.
func (cl *client) send(method, path, auth string, body any) reply {
	cl.t.Helper()
	var payload []byte
	switch b := body.(type) {
	case nil:
	case string:
		payload = []byte(b)
	default:
		payload, _ = json.Marshal(b)
	}
	req := method + " " + api + path + " HTTP/1.1\r\nHost: forge.test:4000\r\nAccept: application/vnd.github+json\r\n"
	if auth != "" {
		req += "Authorization: " + auth + "\r\n"
	}
	if body != nil {
		req += "Content-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(payload)) + "\r\n"
	}
	raw := cl.tester.Do(append([]byte(req+"\r\n"), payload...))
	head, rest, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		cl.t.Fatalf("%s %s: malformed reply %q", method, path, raw)
	}
	lines := strings.Split(string(head), "\r\n")
	r := reply{t: cl.t, what: method + " " + path, header: map[string]string{}, body: bytes.Clone(rest)}
	r.status, _ = strconv.Atoi(lines[0][9:12])
	for _, line := range lines[1:] {
		name, value, _ := strings.Cut(line, ": ")
		r.header[strings.ToLower(name)] = value
	}
	if r.header["x-github-request-id"] == "" {
		cl.t.Errorf("%s: no X-GitHub-Request-Id on a %d", r.what, r.status)
	}
	if r.status == 204 {
		if len(rest) != 0 || r.header["content-length"] != "" {
			cl.t.Errorf("%s: a 204 must have no body and no Content-Length", r.what)
		}
		return r
	}
	if n, err := strconv.Atoi(r.header["content-length"]); err != nil || n != len(rest) {
		cl.t.Fatalf("%s: Content-Length %q, body %d bytes", r.what, r.header["content-length"], len(rest))
	}
	if r.header["content-type"] != "application/json; charset=utf-8" {
		cl.t.Errorf("%s: Content-Type %q", r.what, r.header["content-type"])
	}
	if !json.Valid(rest) {
		cl.t.Fatalf("%s: body is not JSON: %s", r.what, rest)
	}
	return r
}

func (cl *client) get(path string) reply { return cl.send("GET", path, "Bearer "+token, nil) }
func (cl *client) post(path string, body any) reply {
	return cl.send("POST", path, "Bearer "+token, body)
}
func (cl *client) put(path string, body any) reply {
	return cl.send("PUT", path, "Bearer "+token, body)
}
func (cl *client) patch(path string, body any) reply {
	return cl.send("PATCH", path, "Bearer "+token, body)
}
func (cl *client) delete(path string, body any) reply {
	return cl.send("DELETE", path, "Bearer "+token, body)
}

// want fails the test unless the reply has the status; it returns the reply for chaining.
func (r reply) want(status int) reply {
	r.t.Helper()
	if r.status != status {
		r.t.Fatalf("%s: status %d, want %d; body %s", r.what, r.status, status, r.body)
	}
	if status >= 400 && r.object()["message"] == "" {
		r.t.Fatalf("%s: a %d body needs a non-empty message: %s", r.what, status, r.body)
	}
	return r
}

type obj = map[string]any

func (r reply) object() obj {
	r.t.Helper()
	var m obj
	if err := json.Unmarshal(r.body, &m); err != nil || m == nil {
		r.t.Fatalf("%s: body is not a JSON object: %s", r.what, r.body)
	}
	return m
}

func (r reply) array() []obj {
	r.t.Helper()
	var a []obj
	if err := json.Unmarshal(r.body, &a); err != nil || a == nil {
		r.t.Fatalf("%s: body is not a JSON array of objects: %s", r.what, r.body)
	}
	return a
}

func (r reply) message() string { return r.object()["message"].(string) }

// at walks nested objects: at(m, "commit", "sha").
func at(m obj, keys ...string) any {
	var v any = m
	for _, k := range keys {
		v = v.(obj)[k]
	}
	return v
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func isSHA(v any) bool {
	s, ok := v.(string)
	if !ok || len(s) != 40 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

const (
	fileContent    = "amber\nbravo\n  charlie delta\necho\n"
	fileContentAlt = "nickel\ncharlie\ngranite\nhotel india\n"
)

// repo creates a repository with one commit on main (config/token.css) and returns its API path and
// the commit's sha: the fixture nearly every check of the suite starts from.
func (cl *client) repo(name string) (path, initial string) {
	cl.t.Helper()
	created := cl.post("/user/repos", obj{"name": name, "private": true, "description": "the tooling behind the thing"}).want(201).object()
	if created["name"] != name || created["private"] != true {
		cl.t.Fatalf("created repository = %v", created)
	}
	path = "/repos/adaiversen/" + name
	put := cl.put(path+"/contents/config/token.css", obj{"message": "Tidy issue pagination", "content": b64(fileContent)}).want(201).object()
	if !isSHA(at(put, "content", "sha")) || at(put, "content", "encoding") != "base64" || !isSHA(at(put, "commit", "sha")) {
		cl.t.Fatalf("first contents write = %v", put)
	}
	return path, at(put, "commit", "sha").(string)
}

func TestProtocol(t *testing.T) {
	cl := newClient(t)
	path, _ := cl.repo("amber-thistle-protocol")

	// Authentication comes first, on every route, known or not.
	for _, p := range []string{"/", "", "/meta", path, "/no-such-route"} {
		if r := cl.send("GET", p, "", nil).want(401); r.message() != "Requires authentication" {
			t.Errorf("anonymous GET %s: %s", p, r.body)
		}
		if r := cl.send("GET", p, "Bearer wrong", nil).want(401); r.message() != "Bad credentials" {
			t.Errorf("wrong token GET %s: %s", p, r.body)
		}
	}
	if cl.send("POST", "/user/repos", "", `{"name":`).status != 401 {
		t.Error("an anonymous request with a broken body must be a 401, not a 400")
	}
	// "token <t>" and any case of the scheme are accepted.
	cl.send("GET", "/user", "token "+token, nil).want(200)
	cl.send("GET", "/user", "bearer "+token, nil).want(200)

	root := cl.get("/").want(200)
	if root.header["x-oauth-scopes"] == "" || root.object()["current_user_url"] != origin+"/api/v3/user" {
		t.Errorf("root: headers %v body %s", root.header, root.body)
	}
	cl.get("").want(200)
	if v := cl.get("/meta").want(200).object()["installed_version"]; v == "" || v == nil {
		t.Error("meta has no installed_version")
	}
	if u := cl.get("/user").want(200).object(); u["login"] != "adaiversen" || u["id"] != float64(1000) {
		t.Errorf("user = %v", u)
	}
	cl.get("/users/adaiversen").want(200)
	cl.get("/users/olgabauer").want(404)

	// Unknown routes, and known paths with a method they do not have, are JSON 404s.
	for _, p := range []string{"/no-such-route/amber-thistle", "/repos", "/repos/adaiversen", path + "/nope", path + "/issues/1/nope"} {
		if r := cl.get(p).want(404); r.message() != "Not Found" {
			t.Errorf("GET %s: %s", p, r.body)
		}
	}
	cl.delete("/user/repos", nil).want(404)
	cl.put(path, obj{}).want(404)
	// HEAD is GET without the body.
	head := cl.tester.Do([]byte("HEAD /api/v3" + path + " HTTP/1.1\r\nHost: forge.test:4000\r\nAuthorization: Bearer " + token + "\r\n\r\n"))
	if !bytes.HasPrefix(head, []byte("HTTP/1.1 200 OK\r\n")) || !bytes.HasSuffix(head, []byte("\r\n\r\n")) || !bytes.Contains(head, []byte("Content-Length: ")) {
		t.Errorf("HEAD reply = %q", head)
	}

	// A broken body is a 400 once the route and the repository are known.
	for _, body := range []string{`{"name":`, ``, `[]`, `"text"`, `{"name":"x"} trailing`, `nonsense`} {
		if r := cl.post("/user/repos", body).want(400); r.message() != "Problems parsing JSON" {
			t.Errorf("POST /user/repos %q: %s", body, r.body)
		}
	}
	cl.post("/repos/adaiversen/missing/issues", `{"title":`).want(404)
	cl.post(path+"/issues", `{"title":`).want(400)
	cl.post(path+"/issues", `{"title":"t","body":"unterminated`).want(400)
	// A value of the wrong JSON type is a 422, not a parse error.
	cl.post("/user/repos", `{"name":17}`).want(422)
	cl.post(path+"/issues", `{"title":"t","labels":"bug"}`).want(422)
	cl.post(path+"/issues", `{"title":"t","body":17}`).want(422)
}

func TestRepositories(t *testing.T) {
	cl := newClient(t)
	const name = "amber-thistle-repos-create"
	created := cl.post("/user/repos", obj{"name": name, "private": true, "description": "the tooling behind the thing"}).want(201).object()
	path := "/repos/adaiversen/" + name
	got := cl.get(path).want(200).object()
	for _, m := range []obj{created, got} {
		if m["name"] != name || m["private"] != true || m["default_branch"] != "main" || m["full_name"] != "adaiversen/"+name ||
			m["description"] != "the tooling behind the thing" || at(m, "owner", "login") != "adaiversen" ||
			m["url"] != origin+"/api/v3/repos/adaiversen/"+name || m["clone_url"] != origin+"/adaiversen/"+name+".git" {
			t.Errorf("repository = %v", m)
		}
	}
	if got["id"] != created["id"] || cl.get(path).want(200).object()["id"] != created["id"] {
		t.Error("the id must be stable across reads")
	}
	// Owner and name are matched without case; another owner never sees the repository.
	cl.get("/repos/AdaIversen/AMBER-thistle-repos-create").want(200)
	cl.get("/repos/olgabauer/" + name).want(404)
	cl.get("/repos/adaiversen/amber-thistle-never-created").want(404)

	// A duplicate is a 422 that says "already exists" early in its body, and changes nothing.
	dup := cl.post("/user/repos", obj{"name": name, "description": "git conformance", "private": true, "auto_init": false}).want(422)
	if !strings.Contains(string(dup.body[:min(len(dup.body), 400)]), "already exists") {
		t.Errorf("duplicate create: %s", dup.body)
	}
	if cl.get(path).want(200).object()["description"] != "the tooling behind the thing" {
		t.Error("a refused create changed the repository")
	}
	cl.post("/user/repos", obj{"description": "no name"}).want(422)
	cl.post("/user/repos", obj{"name": "bad name"}).want(422)

	// No description is null; public by default; visibility is understood.
	plain := cl.post("/user/repos", obj{"name": "plain"}).want(201).object()
	if v, has := plain["description"]; !has || v != nil || plain["private"] != false {
		t.Errorf("plain repository = %v", plain)
	}
	if cl.post("/user/repos", obj{"name": "by-visibility", "visibility": "private"}).want(201).object()["private"] != true {
		t.Error("visibility private was ignored")
	}

	// PATCH: description, null description, visibility, rename.
	patched := cl.patch(path, obj{"description": "the tooling behind the thing (updated)"}).want(200).object()
	if patched["description"] != "the tooling behind the thing (updated)" || cl.get(path).object()["description"] != patched["description"] {
		t.Errorf("patched = %v", patched)
	}
	if m := cl.patch(path, `{"description":null,"private":false}`).want(200).object(); m["description"] != nil || m["private"] != false {
		t.Errorf("patched = %v", m)
	}
	cl.patch(path, `{"description":`).want(400)
	cl.patch("/repos/adaiversen/missing", obj{"description": "x"}).want(404)
	if cl.patch(path, obj{"name": "renamed"}).want(200).object()["name"] != "renamed" {
		t.Error("rename failed")
	}
	cl.get(path).want(404)
	cl.patch("/repos/adaiversen/renamed", obj{"name": "plain"}).want(422)

	// DELETE: 204, then the repository is gone and its name is free.
	cl.delete("/repos/adaiversen/renamed", nil).want(204)
	cl.get("/repos/adaiversen/renamed").want(404)
	cl.delete("/repos/adaiversen/renamed", nil).want(404)
	again := cl.post("/user/repos", obj{"name": "renamed"}).want(201).object()
	if again["id"] == created["id"] {
		t.Error("a recreated repository must get a new id")
	}

	// auto_init starts the repository with a README on main.
	cl.post("/user/repos", obj{"name": "with-readme", "auto_init": true, "description": "hello"}).want(201)
	readme := cl.get("/repos/adaiversen/with-readme/contents/README.md").want(200).object()
	if raw, _ := base64.StdEncoding.DecodeString(readme["content"].(string)); string(raw) != "# with-readme\n\nhello\n" {
		t.Errorf("README = %q", raw)
	}
}

// walk follows Link rel="next" from path and returns the names seen and the number of pages.
func (cl *client) walk(path string, perPage int) (names []string, pages int) {
	cl.t.Helper()
	next := path
	for next != "" {
		if pages++; pages > 20 {
			cl.t.Fatal("pagination does not end")
		}
		r := cl.get(next).want(200)
		items := r.array()
		if len(items) > perPage {
			cl.t.Fatalf("%s: %d items on a page of %d", next, len(items), perPage)
		}
		for _, it := range items {
			names = append(names, fmt.Sprint(it["name"]))
		}
		next = ""
		for _, part := range strings.Split(r.header["link"], ",") {
			url, rel, _ := strings.Cut(strings.TrimSpace(part), ">; ")
			if rel != `rel="next"` {
				continue
			}
			url = strings.TrimPrefix(url, "<")
			if !strings.HasPrefix(url, origin+api+"/") {
				cl.t.Fatalf("next link left the origin: %q", r.header["link"])
			}
			next = strings.TrimPrefix(url, origin+api)
		}
	}
	return names, pages
}

func TestPagination(t *testing.T) {
	cl := newClient(t)
	var want []string
	for i := 0; i < 35; i++ {
		name := fmt.Sprintf("repo-%02d", i)
		want = append(want, name)
		cl.post("/user/repos", obj{"name": name, "private": i%2 == 0}).want(201)
	}
	names, pages := cl.walk("/user/repos?per_page=10", 10)
	if pages != 4 || strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("walked %d pages: %v", pages, names)
	}
	first := cl.get("/user/repos?per_page=10")
	if link := first.header["link"]; link != `<`+origin+`/api/v3/user/repos?per_page=10&page=2>; rel="next", <`+origin+`/api/v3/user/repos?per_page=10&page=4>; rel="last"` {
		t.Errorf("first page Link = %q", link)
	}
	// Other query parameters survive in the links; only page is replaced.
	mid := cl.get("/user/repos?page=2&type=owner&per_page=10&x=a,b")
	if link := mid.header["link"]; link != `<`+origin+`/api/v3/user/repos?type=owner&per_page=10&x=a%2Cb&page=1>; rel="prev", `+
		`<`+origin+`/api/v3/user/repos?type=owner&per_page=10&x=a%2Cb&page=3>; rel="next", `+
		`<`+origin+`/api/v3/user/repos?type=owner&per_page=10&x=a%2Cb&page=4>; rel="last", `+
		`<`+origin+`/api/v3/user/repos?type=owner&per_page=10&x=a%2Cb&page=1>; rel="first"` {
		t.Errorf("middle page Link = %q", link)
	}
	if link := cl.get("/user/repos?per_page=10&page=4").header["link"]; strings.Contains(link, `rel="next"`) || !strings.Contains(link, `rel="prev"`) {
		t.Errorf("last page Link = %q", link)
	}
	if r := cl.get("/user/repos?per_page=100"); r.header["link"] != "" || len(r.array()) != 35 {
		t.Errorf("one page: Link %q, %d items", r.header["link"], len(r.array()))
	}
	// Defaults and limits: 30 per page, at most 100, a page past the end is empty.
	if n := len(cl.get("/user/repos").array()); n != 30 {
		t.Errorf("default page has %d items", n)
	}
	if n := len(cl.get("/user/repos?per_page=1000").array()); n != 35 {
		t.Errorf("per_page=1000 gave %d items", n)
	}
	if r := cl.get("/user/repos?page=9").want(200); string(r.body) != "[]" {
		t.Errorf("page past the end = %s", r.body)
	}
	if n := len(cl.get("/user/repos?per_page=abc&page=-1").array()); n != 30 {
		t.Errorf("bad paging values gave %d items", n)
	}
	// Filters and explicit orders.
	if names, _ := cl.walk("/user/repos?visibility=private&per_page=10", 10); len(names) != 18 || names[1] != "repo-02" {
		t.Errorf("private repositories: %v", names)
	}
	if names, _ := cl.walk("/user/repos?sort=full_name&direction=desc&per_page=100", 100); names[0] != "repo-34" || names[34] != "repo-00" {
		t.Errorf("sorted by name, descending: %v", names)
	}
	if names, _ := cl.walk("/user/repos?sort=created&per_page=100", 100); names[0] != "repo-34" {
		t.Errorf("sorted by creation, newest first: %v", names)
	}
	if names, _ := cl.walk("/users/adaiversen/repos?per_page=100", 100); len(names) != 35 {
		t.Errorf("user repositories: %v", names)
	}
}

func TestContents(t *testing.T) {
	cl := newClient(t)
	cl.post("/user/repos", obj{"name": "empty"}).want(201)
	if r := cl.get("/repos/adaiversen/empty/contents/config/token.css").want(404); r.message() != "This repository is empty." {
		t.Errorf("contents of an empty repository: %s", r.body)
	}
	cl.get("/repos/adaiversen/empty/contents").want(404)

	path, initial := cl.repo("amber-thistle-contents")
	file := path + "/contents/config/token.css"

	got := cl.get(file).want(200).object()
	raw, err := base64.StdEncoding.DecodeString(got["content"].(string))
	if err != nil || string(raw) != fileContent || got["type"] != "file" || got["encoding"] != "base64" || got["size"] != float64(len(fileContent)) ||
		got["name"] != "token.css" || got["path"] != "config/token.css" || !isSHA(got["sha"]) {
		t.Fatalf("file = %v (%v)", got, err)
	}
	// The blob sha is git's: sha1("blob <len>\0<bytes>").
	if want := fmt.Sprintf("%x", sha1.Sum([]byte("blob "+strconv.Itoa(len(fileContent))+"\x00"+fileContent))); got["sha"] != want {
		t.Errorf("blob sha = %v, want %s", got["sha"], want)
	}
	s0 := got["sha"].(string)

	// Create a second file: 201, readable, same sha as the write reported.
	created := cl.put(path+"/contents/config/token-created.css", obj{"message": "Tidy issue pagination", "content": b64(fileContentAlt)}).want(201).object()
	read := cl.get(path + "/contents/config/token-created.css").want(200).object()
	if read["sha"] != at(created, "content", "sha") || read["size"] != float64(len(fileContentAlt)) {
		t.Errorf("created %v, read %v", created["content"], read)
	}
	if at(created, "commit", "message") != "Tidy issue pagination" || at(created, "commit", "parents").([]any)[0].(obj)["sha"] != initial {
		t.Errorf("commit of the write = %v", created["commit"])
	}

	// Directory listing: an array sorted by name; the root too.
	cl.put(path+"/contents/config/sub/deep.txt", obj{"message": "deep", "content": b64("x")}).want(201)
	dir := cl.get(path + "/contents/config").want(200).array()
	var names []string
	for _, e := range dir {
		names = append(names, e["type"].(string)+":"+e["name"].(string)+":"+e["path"].(string))
		if !isSHA(e["sha"]) {
			t.Errorf("entry without sha: %v", e)
		}
	}
	if strings.Join(names, " ") != "dir:sub:config/sub file:token-created.css:config/token-created.css file:token.css:config/token.css" {
		t.Errorf("directory = %v", names)
	}
	if root := cl.get(path + "/contents").want(200).array(); len(root) != 1 || root[0]["type"] != "dir" || root[0]["name"] != "config" {
		t.Errorf("root = %v", root)
	}
	cl.get(path + "/contents/config/").want(200).array()
	cl.get(path + "/contents/config/missing.css").want(404)
	if r := cl.get(file + "?ref=no-such-branch").want(404); r.message() != "No commit found for the ref no-such-branch" {
		t.Errorf("unknown ref: %s", r.body)
	}
	if cl.get(file + "?ref=" + initial).want(200).object()["sha"] != s0 {
		t.Error("reading at a commit sha failed")
	}

	// Update: needs the current sha (422 without, 409 when stale), 200 when it matches.
	if r := cl.put(file, obj{"message": "m", "content": b64(fileContentAlt)}).want(422); !strings.Contains(r.message(), `"sha" wasn't supplied`) {
		t.Errorf("update without sha: %s", r.body)
	}
	updated := cl.put(file, obj{"message": "Tidy issue pagination", "content": b64(fileContentAlt), "sha": s0}).want(200).object()
	s1 := at(updated, "content", "sha").(string)
	if s1 == s0 {
		t.Error("the update did not change the blob")
	}
	// s0 is now stale, even though the content being written hashes to s0.
	if r := cl.put(file, obj{"message": "Tidy issue pagination", "content": b64(fileContent), "sha": s0}).want(409); r.message() != "config/token.css does not match "+s0 {
		t.Errorf("stale sha: %s", r.body)
	}
	cl.put(file, obj{"message": "upper", "content": b64(fileContent), "sha": strings.ToUpper(s1)}).want(200)

	// Validation of the body.
	cl.put(file, obj{"message": "m"}).want(422)
	cl.put(file, obj{"content": b64("x")}).want(422)
	if r := cl.put(file, obj{"message": "m", "content": "***"}).want(422); r.message() != "content is not valid Base64" {
		t.Errorf("bad base64: %s", r.body)
	}
	cl.put(path+"/contents/config", obj{"message": "m", "content": b64("x")}).want(422)
	cl.put(path+"/contents/config/token.css/below", obj{"message": "m", "content": b64("x")}).want(422)
	cl.put(file, `{"message":`).want(400)
	cl.put("/repos/adaiversen/missing/contents/a.txt", obj{"message": "m", "content": b64("x")}).want(404)
	// Wrapped and unpadded base64 are accepted; an empty file is a file.
	cl.put(path+"/contents/wrapped.txt", obj{"message": "m", "content": "aGVs\nbG8g\r\nd29y bGQ"}).want(201)
	if w := cl.get(path + "/contents/wrapped.txt").object(); w["content"] != b64("hello world") {
		t.Errorf("wrapped content = %v", w["content"])
	}
	if e := cl.put(path+"/contents/empty.txt", obj{"message": "m", "content": ""}).want(201).object(); at(e, "content", "size") != float64(0) {
		t.Errorf("empty file = %v", e["content"])
	}
	// A path with characters that need encoding round-trips.
	cl.put(path+"/contents/docs/read%20me%2B.md", obj{"message": "m", "content": b64("x")}).want(201)
	if m := cl.get(path + "/contents/docs/read%20me%2B.md").want(200).object(); m["path"] != "docs/read me+.md" {
		t.Errorf("encoded path = %v", m["path"])
	}

	// Delete: needs the sha, 409 when stale, 200 with content null, then the file is gone.
	current := cl.get(file).object()["sha"].(string)
	cl.delete(file, obj{"message": "rm"}).want(422)
	cl.delete(file, obj{"message": "rm", "sha": s0 + "0"}).want(409)
	cl.delete(path+"/contents/config/missing.css", obj{"message": "rm", "sha": current}).want(404)
	del := cl.delete(file, obj{"message": "Tidy issue pagination", "sha": current}).want(200).object()
	if v, has := del["content"]; !has || v != nil || !isSHA(at(del, "commit", "sha")) {
		t.Errorf("delete reply = %v", del)
	}
	cl.get(file).want(404)
	// The parameters may come in the query instead of a body.
	w := cl.get(path + "/contents/wrapped.txt").object()["sha"].(string)
	cl.delete(path+"/contents/wrapped.txt?message=rm+it&sha="+w, nil).want(200)
	cl.get(path + "/contents/wrapped.txt").want(404)
}

func TestDeleteLastFile(t *testing.T) {
	cl := newClient(t)
	path, _ := cl.repo("amber-thistle-last-file")
	file := path + "/contents/config/token.css"
	sha := cl.get(file).want(200).object()["sha"]
	del := cl.delete(file, obj{"message": "Tidy issue pagination", "sha": sha}).want(200).object()
	if at(del, "commit", "tree", "sha") != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Errorf("deleting the last file must leave the empty tree: %v", del["commit"])
	}
	cl.get(file).want(404)
	if root := cl.get(path + "/contents").want(200); string(root.body) != "[]" {
		t.Errorf("root after deleting the last file = %s", root.body)
	}
	cl.get(path + "/contents/config").want(404)
	// The repository still works: a new file lands on top.
	cl.put(file, obj{"message": "again", "content": b64("x")}).want(201)
}

func TestBranchesRefsCommits(t *testing.T) {
	cl := newClient(t)
	cl.post("/user/repos", obj{"name": "empty"}).want(201)
	if r := cl.get("/repos/adaiversen/empty/commits").want(409); r.message() != "Git Repository is empty." {
		t.Errorf("commits of an empty repository: %s", r.body)
	}
	if r := cl.get("/repos/adaiversen/empty/branches").want(200); string(r.body) != "[]" {
		t.Errorf("branches of an empty repository = %s", r.body)
	}

	path, initial := cl.repo("amber-thistle-git")
	ref := cl.post(path+"/git/refs", obj{"ref": "refs/heads/develop-branches", "sha": initial}).want(201).object()
	if ref["ref"] != "refs/heads/develop-branches" || at(ref, "object", "sha") != initial || at(ref, "object", "type") != "commit" {
		t.Errorf("created ref = %v", ref)
	}
	if r := cl.post(path+"/git/refs", obj{"ref": "refs/heads/develop-branches", "sha": initial}).want(422); r.message() != "Reference already exists" {
		t.Errorf("duplicate ref: %s", r.body)
	}
	if r := cl.post(path+"/git/refs", obj{"ref": "refs/heads/other", "sha": strings.Repeat("ab", 20)}).want(422); r.message() != "Object does not exist" {
		t.Errorf("unknown sha: %s", r.body)
	}
	cl.post(path+"/git/refs", obj{"ref": "refs/heads/other", "sha": "not-a-sha"}).want(422)
	if r := cl.post(path+"/git/refs", obj{"ref": "develop", "sha": initial}).want(422); r.message() != "Reference name is invalid" {
		t.Errorf("invalid ref name: %s", r.body)
	}
	cl.post(path+"/git/refs", `{"ref":`).want(400)
	// Branch names with slashes, raw and percent-encoded.
	cl.post(path+"/git/refs", obj{"ref": "refs/heads/release/1.0", "sha": initial}).want(201)

	branches := cl.get(path + "/branches").want(200).array()
	var names []string
	for _, b := range branches {
		names = append(names, b["name"].(string))
		if at(b, "commit", "sha") != initial {
			t.Errorf("branch = %v", b)
		}
	}
	if strings.Join(names, " ") != "develop-branches main release/1.0" {
		t.Errorf("branches = %v", names)
	}
	for _, p := range []string{"/branches/release/1.0", "/branches/release%2F1.0", "/branches/main"} {
		if b := cl.get(path + p).want(200).object(); at(b, "commit", "sha") != initial {
			t.Errorf("GET %s = %v", p, b)
		}
	}
	if r := cl.get(path + "/branches/missing").want(404); r.message() != "Branch not found" {
		t.Errorf("missing branch: %s", r.body)
	}
	for _, p := range []string{"/git/refs/heads/release/1.0", "/git/ref/heads/release%2F1.0", "/git/ref/heads/main", "/git/refs/heads/main"} {
		if g := cl.get(path + p).want(200).object(); at(g, "object", "sha") != initial || !strings.HasPrefix(g["ref"].(string), "refs/heads/") {
			t.Errorf("GET %s = %v", p, g)
		}
	}
	cl.get(path + "/git/ref/heads/missing").want(404)
	cl.get(path + "/git/refs/heads/missing").want(404)
	if n := len(cl.get(path + "/git/refs/heads").want(200).array()); n != 3 {
		t.Errorf("refs under heads: %d", n)
	}
	if n := len(cl.get(path + "/git/refs").want(200).array()); n != 3 {
		t.Errorf("all refs: %d", n)
	}
	if n := len(cl.get(path + "/git/matching-refs/heads/rel").want(200).array()); n != 1 {
		t.Errorf("matching refs: %d", n)
	}
	if r := cl.get(path + "/git/matching-refs/tags").want(200); string(r.body) != "[]" {
		t.Errorf("matching nothing = %s", r.body)
	}

	// A commit on a branch shows in that branch's history, not in the default branch's.
	put := cl.put(path+"/contents/config/token-commits.css", obj{"message": "Tidy issue pagination (commits)", "content": b64(fileContentAlt), "branch": "develop-branches"}).want(201).object()
	tip := at(put, "commit", "sha").(string)
	onBranch := cl.get(path + "/commits?sha=develop-branches").want(200).array()
	if len(onBranch) != 2 || onBranch[0]["sha"] != tip || at(onBranch[0], "commit", "message") != "Tidy issue pagination (commits)" || onBranch[1]["sha"] != initial {
		t.Errorf("branch history = %v", onBranch)
	}
	if def := cl.get(path + "/commits").want(200).array(); len(def) != 1 || def[0]["sha"] != initial {
		t.Errorf("default history = %v", def)
	}
	if r := cl.get(path + "/commits?sha=missing").want(404); r.message() != "No commit found for SHA: missing" {
		t.Errorf("unknown sha: %s", r.body)
	}
	if cl.get(path + "/commits?sha=" + tip).want(200).array()[0]["sha"] != tip {
		t.Error("history by commit sha failed")
	}
	// Paginated history: one per page, with a next link while more remain.
	page1 := cl.get(path + "/commits?sha=develop-branches&per_page=1").want(200)
	if len(page1.array()) != 1 || page1.header["link"] != `<`+origin+`/api/v3/repos/adaiversen/amber-thistle-git/commits?sha=develop-branches&per_page=1&page=2>; rel="next"` {
		t.Errorf("history page 1: Link %q", page1.header["link"])
	}
	if page2 := cl.get(path + "/commits?sha=develop-branches&per_page=1&page=2").want(200); page2.array()[0]["sha"] != initial || strings.Contains(page2.header["link"], "next") {
		t.Errorf("history page 2: %s Link %q", page2.body, page2.header["link"])
	}
	for _, p := range []string{"/commits/" + tip, "/commits/develop-branches", "/commits/heads/develop-branches"} {
		if cm := cl.get(path + p).want(200).object(); cm["sha"] != tip {
			t.Errorf("GET %s = %v", p, cm)
		}
	}
	cl.get(path + "/commits/missing").want(404)
	// The branch tip moved; the ref API shows it at once.
	if cl.get(path + "/git/ref/heads/develop-branches").object()["object"].(obj)["sha"] != tip {
		t.Error("the ref did not follow the commit")
	}

	// Moving and deleting refs.
	if r := cl.patch(path+"/git/refs/heads/develop-branches", obj{"sha": initial}).want(422); r.message() != "Update is not a fast forward" {
		t.Errorf("non fast-forward: %s", r.body)
	}
	cl.patch(path+"/git/refs/heads/develop-branches", obj{"sha": initial, "force": true}).want(200)
	cl.patch(path+"/git/refs/heads/main", obj{"sha": tip}).want(200)
	cl.patch(path+"/git/refs/heads/missing", obj{"sha": tip}).want(422)
	cl.delete(path+"/git/refs/heads/release/1.0", nil).want(204)
	cl.get(path + "/branches/release/1.0").want(404)
	cl.delete(path+"/git/refs/heads/release/1.0", nil).want(422)
}

func TestLabelsIssuesComments(t *testing.T) {
	cl := newClient(t)
	path, _ := cl.repo("amber-thistle-issues")
	const (
		title = "Incorrect GraphQL node ids"
		body  = "A minimal case is attached but the second request fails. The same flow works over the API."
	)

	label := cl.post(path+"/labels", obj{"name": "bug", "color": "ededed"}).want(201).object()
	if label["name"] != "bug" || label["color"] != "ededed" || label["description"] != nil {
		t.Errorf("label = %v", label)
	}
	cl.post(path+"/labels", obj{"name": "bug"}).want(422)
	cl.post(path+"/labels", obj{"color": "ffffff"}).want(422)
	cl.post(path+"/labels", obj{"name": "help wanted", "color": "#00FF00", "description": "needs a hand"}).want(201)
	if l := cl.get(path + "/labels/help%20wanted").want(200).object(); l["color"] != "00ff00" || l["description"] != "needs a hand" {
		t.Errorf("label = %v", l)
	}
	cl.get(path + "/labels/missing").want(404)
	if n := len(cl.get(path + "/labels").want(200).array()); n != 2 {
		t.Errorf("%d labels", n)
	}

	// Create: 201, open, echoes title and body byte for byte, carries its labels.
	first := cl.post(path+"/issues", obj{"title": title, "body": body, "labels": []string{"bug"}}).want(201).object()
	if first["number"] != float64(1) || first["title"] != title || first["body"] != body || first["state"] != "open" ||
		first["labels"].([]any)[0].(obj)["name"] != "bug" || first["comments"] != float64(0) || first["closed_at"] != nil {
		t.Errorf("issue = %v", first)
	}
	// Without a body the body is "", never null; numbers increase by one.
	second := cl.post(path+"/issues", obj{"title": title + " 2"}).want(201).object()
	if second["number"] != float64(2) || second["body"] != "" || len(second["labels"].([]any)) != 0 {
		t.Errorf("second issue = %v", second)
	}
	if m := cl.post(path+"/issues", `{"title":"null body","body":null,"labels":null}`).want(201).object(); m["body"] != "" {
		t.Errorf("null body = %v", m["body"])
	}
	// Text with quotes, escapes and non-ASCII characters round-trips.
	odd := "quote \" backslash \\ newline \n tab \t <b>&amp;</b> héllo 世界 \U0001F600"
	if m := cl.post(path+"/issues", obj{"title": odd, "body": odd}).want(201).object(); m["title"] != odd || m["body"] != odd {
		t.Errorf("odd text = %v", m)
	}
	// Labels given as objects; label names match without case.
	if m := cl.post(path+"/issues", `{"title":"objects","labels":[{"name":"BUG"},"help wanted"]}`).want(201).object(); len(m["labels"].([]any)) != 2 {
		t.Errorf("labels as objects = %v", m["labels"])
	}

	// An unknown label is a 422 and creates neither the issue nor the label.
	bad := cl.post(path+"/issues", obj{"title": title, "body": body, "labels": []string{"bug-not-a-label"}}).want(422)
	if bad.message() != "Validation Failed" || !strings.Contains(string(bad.body), `"value":"bug-not-a-label"`) {
		t.Errorf("unknown label: %s", bad.body)
	}
	cl.get(path + "/labels/bug-not-a-label").want(404)
	cl.post(path+"/issues", obj{"body": "no title"}).want(422)
	cl.post(path+"/issues", obj{"title": ""}).want(422)
	cl.post(path+"/issues", `{}`).want(422)
	if next := cl.post(path+"/issues", obj{"title": "after the failures"}).want(201).object(); next["number"] != float64(6) {
		t.Errorf("a refused create consumed a number: %v", next["number"])
	}

	// Get one; unknown numbers and non-numbers are 404s.
	if got := cl.get(path + "/issues/1").want(200).object(); got["number"] != float64(1) || got["title"] != title || got["body"] != body || got["id"] != first["id"] {
		t.Errorf("get issue = %v", got)
	}
	for _, n := range []string{"0", "99", "999999999", "99999999999999999999999", "abc", "1x", "-1"} {
		cl.get(path + "/issues/" + n).want(404)
	}

	// Close: 200, state closed, closed_at set, and it persists; reopen undoes it.
	closed := cl.patch(path+"/issues/2", obj{"state": "closed"}).want(200).object()
	if closed["number"] != float64(2) || closed["state"] != "closed" || closed["state_reason"] != "completed" || closed["closed_at"] == nil {
		t.Errorf("closed issue = %v", closed)
	}
	if got := cl.get(path + "/issues/2").want(200).object(); got["state"] != "closed" {
		t.Errorf("close did not persist: %v", got)
	}
	cl.patch(path+"/issues/2", obj{"state": "merged"}).want(422)
	cl.patch(path+"/issues/2", `{"state":`).want(400)
	cl.patch(path+"/issues/99", obj{"state": "closed"}).want(404)
	if m := cl.patch(path+"/issues/1", obj{"title": "new title", "body": nil, "labels": []string{}}).want(200).object(); m["title"] != "new title" || m["body"] != "" || len(m["labels"].([]any)) != 0 || m["state"] != "open" {
		t.Errorf("edited issue = %v", m)
	}
	cl.patch(path+"/issues/1", obj{"labels": []string{"bug-not-a-label"}}).want(422)
	cl.patch(path+"/issues/1", obj{"title": title, "body": body, "labels": []string{"bug"}}).want(200)

	// Lists: open by default, newest first; state, sort and direction as asked.
	numbers := func(q string) string {
		var out []string
		for _, is := range cl.get(path + "/issues" + q).want(200).array() {
			if is["title"] == nil || is["body"] == nil || is["labels"] == nil {
				t.Errorf("list item without the issue shape: %v", is)
			}
			out = append(out, fmt.Sprint(is["number"])+":"+is["state"].(string))
		}
		return strings.Join(out, " ")
	}
	for q, want := range map[string]string{
		"":                         "6:open 5:open 4:open 3:open 1:open",
		"?state=open&per_page=100": "6:open 5:open 4:open 3:open 1:open",
		"?state=closed":            "2:closed",
		"?state=all&direction=asc": "1:open 2:closed 3:open 4:open 5:open 6:open",
		"?state=open&sort=created&direction=asc&per_page=100":  "1:open 3:open 4:open 5:open 6:open",
		"?state=open&sort=created&direction=desc&per_page=100": "6:open 5:open 4:open 3:open 1:open",
		"?labels=bug":                  "5:open 1:open",
		"?labels=bug,help+wanted":      "5:open",
		"?state=all&per_page=2&page=2": "4:open 3:open",
		"?state=all&sort=updated":      "1:open 2:closed 6:open 5:open 4:open 3:open",
	} {
		if got := numbers(q); got != want {
			t.Errorf("GET /issues%s = %s, want %s", q, got, want)
		}
	}
	cl.get(path + "/issues?state=bogus").want(422)
	if link := cl.get(path + "/issues?state=all&per_page=2").header["link"]; !strings.Contains(link, "/issues?state=all&per_page=2&page=2>; rel=\"next\"") {
		t.Errorf("issues Link = %q", link)
	}
	if r := cl.get("/repos/adaiversen/amber-thistle-never-created/issues").want(404); r.message() != "Not Found" {
		t.Errorf("issues of a missing repository: %s", r.body)
	}

	// Comments: 201 with id and the body as posted; listed in creation order; counted on the issue.
	comment := "The same flow works over the API and nothing is written to the log."
	c1 := cl.post(path+"/issues/1/comments", obj{"body": comment}).want(201).object()
	if c1["id"] != float64(1) || c1["body"] != comment {
		t.Errorf("comment = %v", c1)
	}
	cl.post(path+"/issues/1/comments", obj{"body": "second"}).want(201)
	list := cl.get(path + "/issues/1/comments").want(200).array()
	if len(list) != 2 || list[0]["id"] != c1["id"] || list[0]["body"] != comment || list[1]["body"] != "second" {
		t.Errorf("comments = %v", list)
	}
	if r := cl.get(path + "/issues/3/comments").want(200); string(r.body) != "[]" {
		t.Errorf("no comments = %s", r.body)
	}
	if got := cl.get(path + "/issues/1").object(); got["comments"] != float64(2) {
		t.Errorf("the issue did not count its comments: %v", got["comments"])
	}
	cl.post(path+"/issues/1/comments", obj{}).want(422)
	cl.post(path+"/issues/1/comments", obj{"body": ""}).want(422)
	cl.post(path+"/issues/1/comments", `{"body":`).want(400)
	cl.post(path+"/issues/99/comments", obj{"body": "x"}).want(404)
	cl.get(path + "/issues/99/comments").want(404)
	if got := cl.get(path + "/issues/comments/1").want(200).object(); got["body"] != comment {
		t.Errorf("comment by id = %v", got)
	}
	cl.get(path + "/issues/comments/99").want(404)
	if n := len(cl.get(path + "/issues/1/labels").want(200).array()); n != 1 {
		t.Errorf("issue labels: %d", n)
	}

	// Renaming and deleting a label shows on the issues that carry it.
	cl.patch(path+"/labels/bug", obj{"new_name": "defect", "color": "ff0000"}).want(200)
	if got := cl.get(path + "/issues/1").object(); got["labels"].([]any)[0].(obj)["name"] != "defect" {
		t.Errorf("issue after the label rename = %v", got["labels"])
	}
	cl.delete(path+"/labels/defect", nil).want(204)
	if got := cl.get(path + "/issues/1").object(); len(got["labels"].([]any)) != 0 {
		t.Errorf("issue after the label delete = %v", got["labels"])
	}
	cl.delete(path+"/labels/defect", nil).want(404)

	// Issues disabled: creation is refused.
	cl.patch(path, obj{"has_issues": false}).want(200)
	cl.post(path+"/issues", obj{"title": "x"}).want(410)
}

// TestIssueCache checks that a read after any change shows the change: the issue is served from a
// rendering kept on the issue, so every way of changing what it shows must drop that rendering.
func TestIssueCache(t *testing.T) {
	cl := newClient(t)
	path, _ := cl.repo("cache")
	cl.post(path+"/labels", obj{"name": "bug"}).want(201)
	cl.post(path+"/issues", obj{"title": "t", "body": "b", "labels": []string{"bug"}}).want(201)
	read := func() string { return string(cl.get(path + "/issues/1").want(200).body) }
	last := read()
	if read() != last {
		t.Fatal("two reads of an unchanged issue differ")
	}
	changed := func(what string) {
		t.Helper()
		now := read()
		if now == last {
			t.Errorf("%s: the issue still reads as before", what)
		}
		if inList := cl.get(path + "/issues?state=all").want(200).body; string(inList) != "["+now+"]" {
			t.Errorf("%s: the list shows %s", what, inList)
		}
		last = now
	}
	cl.patch(path+"/issues/1", obj{"title": "t2"}).want(200)
	changed("title edit")
	cl.patch(path+"/issues/1", obj{"body": "b2"}).want(200)
	changed("body edit")
	cl.post(path+"/issues/1/comments", obj{"body": "c"}).want(201)
	changed("comment")
	cl.patch(path+"/labels/bug", obj{"color": "123456"}).want(200)
	changed("label recolour")
	cl.patch(path+"/labels/bug", obj{"new_name": "defect"}).want(200)
	changed("label rename")
	cl.patch(path+"/issues/1", obj{"state": "closed"}).want(200)
	changed("close")
	cl.patch(path+"/issues/1", obj{"state": "open"}).want(200)
	changed("reopen")
	cl.delete(path+"/labels/defect", nil).want(204)
	changed("label delete")
	cl.patch(path, obj{"name": "cache-renamed"}).want(200)
	path = "/repos/adaiversen/cache-renamed"
	changed("repository rename")
	if !strings.Contains(last, origin+"/api/v3/repos/adaiversen/cache-renamed/issues/1") {
		t.Errorf("URLs after the rename: %s", last)
	}
	// A client that reaches the forge under another name gets links on that origin.
	raw := cl.tester.Do([]byte("GET /api/v3" + path + "/issues/1 HTTP/1.1\r\nHost: other.test\r\nAuthorization: Bearer " + token + "\r\n\r\n"))
	if !bytes.Contains(raw, []byte(`"url":"http://other.test/api/v3/repos/adaiversen/cache-renamed/issues/1"`)) {
		t.Errorf("issue for another host: %s", raw)
	}
	if read() != last {
		t.Error("the first host's rendering changed")
	}
	// A deleted and recreated repository starts from nothing.
	cl.delete(path, nil).want(204)
	cl.get(path + "/issues/1").want(404)
	cl.post("/user/repos", obj{"name": "cache-renamed"}).want(201)
	cl.get(path + "/issues/1").want(404)
	if m := cl.post(path+"/issues", obj{"title": "fresh"}).want(201).object(); m["number"] != float64(1) {
		t.Errorf("numbering after recreation: %v", m["number"])
	}
}

// pullFixture is the suite's: a repository with one commit, a branch from it with one new file, and
// an open pull request from that branch into main.
func (cl *client) pullFixture(name string) (path, initial, head string, number float64) {
	cl.t.Helper()
	path, initial = cl.repo(name)
	branch := "develop-" + name
	cl.post(path+"/git/refs", obj{"ref": "refs/heads/" + branch, "sha": initial}).want(201)
	put := cl.put(path+"/contents/config/token-"+name+".css", obj{"message": "Tidy issue pagination (" + name + ")", "content": b64(fileContentAlt), "branch": branch}).want(201).object()
	head = at(put, "commit", "sha").(string)
	pull := cl.post(path+"/pulls", obj{"head": branch, "base": "main", "title": "Improve MCP tool schemas", "body": "It works on a fresh repository."}).want(201).object()
	return path, initial, head, pull["number"].(float64)
}

func TestPulls(t *testing.T) {
	cl := newClient(t)
	path, initial, head, number := cl.pullFixture("pulls")
	one := fmt.Sprintf("%s/pulls/%d", path, int(number))

	got := cl.get(one).want(200).object()
	if got["number"] != number || got["state"] != "open" || got["title"] != "Improve MCP tool schemas" || got["body"] != "It works on a fresh repository." ||
		at(got, "head", "ref") != "develop-pulls" || at(got, "head", "sha") != head || at(got, "base", "ref") != "main" || at(got, "base", "sha") != initial ||
		got["merged"] != false || got["mergeable"] != true || got["merged_at"] != nil || got["merge_commit_sha"] != nil {
		t.Errorf("pull = %v", got)
	}
	for _, n := range []string{"999999999", "0", "abc", "99999999999999999999"} {
		cl.get(path + "/pulls/" + n).want(404)
		cl.put(path+"/pulls/"+n+"/merge", obj{"merge_method": "squash"}).want(404)
		cl.get(path + "/pulls/" + n + "/files").want(404)
	}
	// Issues and pull requests share the numbers but not the routes.
	issue := cl.post(path+"/issues", obj{"title": "an issue"}).want(201).object()
	if issue["number"] != number+1 {
		t.Errorf("issue number after a pull request = %v", issue["number"])
	}
	cl.get(fmt.Sprintf("%s/pulls/%d", path, int(number)+1)).want(404)
	cl.get(fmt.Sprintf("%s/issues/%d", path, int(number))).want(404)
	if strings.Contains(string(cl.get(path+"/issues?state=all").body), "Improve MCP tool schemas") {
		t.Error("the issue list contains a pull request")
	}
	// Comments attach to a pull request through the issues route.
	cl.post(fmt.Sprintf("%s/issues/%d/comments", path, int(number)), obj{"body": "on the pull"}).want(201)
	if n := len(cl.get(fmt.Sprintf("%s/issues/%d/comments", path, int(number))).want(200).array()); n != 1 {
		t.Errorf("pull request comments: %d", n)
	}

	// Validation.
	cl.post(path+"/pulls", obj{"head": "develop-pulls", "base": "main", "title": "again"}).want(422)
	cl.post(path+"/pulls", obj{"head": "missing", "base": "main", "title": "x"}).want(422)
	cl.post(path+"/pulls", obj{"head": "develop-pulls", "base": "missing", "title": "x"}).want(422)
	cl.post(path+"/pulls", obj{"head": "develop-pulls", "base": "main"}).want(422)
	cl.post(path+"/pulls", `{"head":`).want(400)

	// Files: merge base against the head tip, so every commit of the branch counts.
	files := cl.get(one + "/files").want(200).array()
	if len(files) != 1 || files[0]["filename"] != "config/token-pulls.css" || files[0]["status"] != "added" ||
		files[0]["additions"] != float64(4) || files[0]["deletions"] != float64(0) || files[0]["changes"] != float64(4) {
		t.Errorf("files = %v", files)
	}
	cl.put(path+"/contents/config/token-multi-b.css", obj{"message": "second", "content": b64(fileContent), "branch": "develop-pulls"}).want(201)
	if files = cl.get(one + "/files").want(200).array(); len(files) != 2 || files[0]["filename"] != "config/token-multi-b.css" {
		t.Errorf("files after a second commit = %v", files)
	}
	if moved := cl.get(one).object(); at(moved, "head", "sha") == head || moved["commits"] != float64(2) || moved["changed_files"] != float64(2) {
		t.Errorf("the pull request did not follow its branch: %v", moved["head"])
	}
	if r := cl.get(one + "/files?per_page=1"); len(r.array()) != 1 || !strings.Contains(r.header["link"], `rel="next"`) {
		t.Errorf("paged files: %s Link %q", r.body, r.header["link"])
	}

	// Lists.
	if list := cl.get(path + "/pulls").want(200).array(); len(list) != 1 || list[0]["number"] != number || !isSHA(at(list[0], "head", "sha")) {
		t.Errorf("open pulls = %v", list)
	}
	if r := cl.get(path + "/pulls?state=closed").want(200); string(r.body) != "[]" {
		t.Errorf("closed pulls = %s", r.body)
	}
	cl.get(path + "/pulls?state=bogus").want(422)

	// Merge: 200, merged true, a commit on main that is not the head commit.
	cl.get(one + "/merge").want(404)
	merged := cl.put(one+"/merge", obj{"merge_method": "squash"}).want(200).object()
	if merged["merged"] != true || !isSHA(merged["sha"]) || merged["message"] != "Pull Request successfully merged" {
		t.Errorf("merge reply = %v", merged)
	}
	cl.get(one + "/merge").want(204)
	after := cl.get(one).want(200).object()
	if after["state"] != "closed" || after["merged"] != true || after["merged_at"] == nil || after["merged_at"] == "" || after["merge_commit_sha"] != merged["sha"] ||
		at(after, "base", "sha") != initial {
		t.Errorf("merged pull = %v", after)
	}
	history := cl.get(path + "/commits?sha=main").want(200).array()
	if history[0]["sha"] != merged["sha"] || len(history) != 2 || len(history[0]["parents"].([]any)) != 1 {
		t.Errorf("main after the squash merge = %v", history)
	}
	onMain := cl.get(path + "/contents/config/token-pulls.css?ref=main").want(200).object()
	if raw, _ := base64.StdEncoding.DecodeString(onMain["content"].(string)); string(raw) != fileContentAlt {
		t.Errorf("merged content = %q", raw)
	}
	// A merged pull request cannot be merged again, and leaves the default list.
	if r := cl.put(one+"/merge", obj{"merge_method": "squash"}).want(405); r.message() != "Pull Request is not mergeable" {
		t.Errorf("second merge: %s", r.body)
	}
	if r := cl.get(path + "/pulls?per_page=100").want(200); string(r.body) != "[]" {
		t.Errorf("open pulls after the merge = %s", r.body)
	}
	// A second pull request from the old tip; the lists tell them apart.
	cl.post(path+"/git/refs", obj{"ref": "refs/heads/develop-pulls-b", "sha": initial}).want(201)
	cl.put(path+"/contents/config/token-pulls-b.css", obj{"message": "b", "content": b64(fileContentAlt), "branch": "develop-pulls-b"}).want(201)
	second := cl.post(path+"/pulls", obj{"head": "develop-pulls-b", "base": "main", "title": "Improve MCP tool schemas", "body": "b"}).want(201).object()
	all := cl.get(path + "/pulls?state=all&per_page=100").want(200).array()
	if len(all) != 2 || all[0]["number"] != second["number"] || all[1]["number"] != number || all[1]["state"] != "closed" || !isSHA(at(all[1], "base", "sha")) {
		t.Errorf("all pulls = %v", all)
	}
	if open := cl.get(path + "/pulls?per_page=100").want(200).array(); len(open) != 1 || open[0]["number"] != second["number"] {
		t.Errorf("open pulls = %v", open)
	}
	if byHead := cl.get(path + "/pulls?state=all&head=adaiversen:develop-pulls").want(200).array(); len(byHead) != 1 || byHead[0]["number"] != number {
		t.Errorf("pulls by head = %v", byHead)
	}
	// Edit and close through PATCH; a merge with no body at all is a merge commit.
	two := fmt.Sprintf("%s/pulls/%d", path, int(second["number"].(float64)))
	if m := cl.patch(two, obj{"title": "retitled", "state": "closed"}).want(200).object(); m["title"] != "retitled" || m["state"] != "closed" || m["merged"] != false {
		t.Errorf("closed pull = %v", m)
	}
	cl.put(two+"/merge", nil).want(405)
	cl.patch(two, obj{"state": "open"}).want(200)
	cl.put(two+"/merge", obj{"merge_method": "fold"}).want(422)
	cl.put(two+"/merge", obj{"sha": initial}).want(409)
	plain := cl.put(two+"/merge", nil).want(200).object()
	if tip := cl.get(path + "/commits/main").want(200).object(); tip["sha"] != plain["sha"] || len(tip["parents"].([]any)) != 2 {
		t.Errorf("merge commit = %v", tip)
	}
}

// TestMergeConflict is the suite's conflict case: two branches from one commit change the same
// file; both pull requests open, the first merges, the second is then a 405 and writes nothing.
func TestMergeConflict(t *testing.T) {
	cl := newClient(t)
	path, initial := cl.repo("conflict")
	var numbers []int
	for _, side := range []string{"a", "b"} {
		branch := "develop-conflict-" + side
		cl.post(path+"/git/refs", obj{"ref": "refs/heads/" + branch, "sha": initial}).want(201)
		current := cl.get(path + "/contents/config/token.css?ref=" + branch).want(200).object()["sha"]
		cl.put(path+"/contents/config/token.css", obj{"message": "conflict " + side, "content": b64(fileContentAlt + " (" + side + ")"), "branch": branch, "sha": current}).want(200)
		pull := cl.post(path+"/pulls", obj{"head": branch, "base": "main", "title": "Improve MCP tool schemas " + side, "body": "x"}).want(201).object()
		numbers = append(numbers, int(pull["number"].(float64)))
	}
	cl.put(fmt.Sprintf("%s/pulls/%d/merge", path, numbers[0]), obj{"merge_method": "squash"}).want(200)
	mainTip := cl.get(path + "/branches/main").object()["commit"].(obj)["sha"]
	if r := cl.put(fmt.Sprintf("%s/pulls/%d/merge", path, numbers[1]), obj{"merge_method": "squash"}).want(405); r.message() != "Pull Request is not mergeable" {
		t.Errorf("conflicting merge: %s", r.body)
	}
	if cl.get(path + "/branches/main").object()["commit"].(obj)["sha"] != mainTip {
		t.Error("a refused merge moved main")
	}
	second := cl.get(fmt.Sprintf("%s/pulls/%d", path, numbers[1])).want(200).object()
	if second["state"] != "open" || second["mergeable"] != false || second["mergeable_state"] != "dirty" {
		t.Errorf("conflicting pull = %v %v %v", second["state"], second["mergeable"], second["mergeable_state"])
	}
}
