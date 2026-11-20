package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"forge/internal/httpd"
	"forge/internal/render"
	"forge/internal/store"
)

// These tests replay what the conformance suite's MCP client sends (spec/MCP.md section 7, one test
// per check) through the real HTTP engine, against a store seeded the way the suite seeds it over
// REST. Where a check then reads the object back over REST, the test compares the tool's answer
// with the bytes of the renderer every REST handler uses, for the same host.

const (
	testToken = "tok"
	testHost  = "forge.test:4000"
	owner     = store.ViewerLogin
	tag       = "6a09e667" // the suite's scenario tag
)

type fixture struct {
	t      *testing.T
	st     *store.Store
	srv    *httpd.Server
	tester *httpd.Tester
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := store.New()
	surface := New(st)
	// The dispatcher's contract: the store is locked around the surface.
	srv := &httpd.Server{Token: testToken, Handler: func(c *httpd.Ctx) {
		st.Lock()
		defer st.Unlock()
		surface.Serve(c)
	}}
	return &fixture{t: t, st: st, srv: srv, tester: httpd.NewTester(srv)}
}

// request builds a POST /mcp with the headers the suite's client sends. auth is the whole
// Authorization value; empty sends none.
func request(auth, method, body string) []byte {
	var b strings.Builder
	b.WriteString("POST /mcp HTTP/1.1\r\nHost: " + testHost + "\r\n")
	b.WriteString("accept: application/json, text/event-stream\r\ncontent-type: application/json\r\n")
	b.WriteString("mcp-protocol-version: 2026-07-28\r\nmcp-method: " + method + "\r\n")
	if auth != "" {
		b.WriteString("authorization: " + auth + "\r\n")
	}
	b.WriteString("content-length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body)
	return []byte(b.String())
}

type reply struct {
	status int
	header map[string]string
	body   []byte

	// The JSON-RPC response in the body, when there is one.
	members map[string]json.RawMessage
	code    int    // error.code
	message string // error.message
}

func parseReply(t *testing.T, raw []byte) reply {
	t.Helper()
	head, payload, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		t.Fatalf("malformed reply %q", raw)
	}
	lines := strings.Split(string(head), "\r\n")
	r := reply{header: map[string]string{}, body: bytes.Clone(payload)}
	r.status, _ = strconv.Atoi(lines[0][9:12])
	for _, line := range lines[1:] {
		name, value, _ := strings.Cut(line, ": ")
		r.header[strings.ToLower(name)] = value
	}
	if n, err := strconv.Atoi(r.header["content-length"]); err != nil || n != len(payload) {
		t.Fatalf("Content-Length %q, body %d bytes", r.header["content-length"], len(payload))
	}
	if _, has := r.header["mcp-session-id"]; has {
		t.Fatalf("the server handed out a session: %v", r.header)
	}
	if len(payload) == 0 {
		return r
	}
	if ct := r.header["content-type"]; !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type %q", ct)
	}
	if err := json.Unmarshal(payload, &r.members); err != nil {
		t.Fatalf("body is not a JSON object: %s", payload)
	}
	if e, has := r.members["error"]; has {
		var obj struct {
			Code    *int
			Message *string
		}
		if err := json.Unmarshal(e, &obj); err != nil || obj.Code == nil || obj.Message == nil || *obj.Message == "" {
			t.Fatalf("malformed error object: %s", e)
		}
		r.code, r.message = *obj.Code, *obj.Message
		if _, both := r.members["result"]; both {
			t.Fatalf("result and error in one response: %s", payload)
		}
	}
	return r
}

// post sends one body with the token and parses the answer.
func (f *fixture) post(method, body string) reply {
	f.t.Helper()
	return parseReply(f.t, f.tester.Do(request("Bearer "+testToken, method, body)))
}

const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"makefun-github-core-clone-env","version":"1.0.0"},"io.modelcontextprotocol/clientCapabilities":{},"protocolVersion":"2026-07-28"}`

// rpc sends a request the way the suite's client frames it (id is raw JSON; params is the inside of
// the params object, without _meta) and requires a well-formed response under the same id.
func (f *fixture) rpc(id, method, params string) reply {
	f.t.Helper()
	if params != "" {
		params += ","
	}
	r := f.post(method, `{"jsonrpc":"2.0","id":`+id+`,"method":"`+method+`","params":{`+params+meta+`}}`)
	if string(r.members["jsonrpc"]) != `"2.0"` || string(r.members["id"]) != id {
		f.t.Fatalf("%s: response envelope %s", method, r.body)
	}
	return r
}

// connect is the server/discover probe that opens every check.
func (f *fixture) connect() reply {
	f.t.Helper()
	r := f.rpc(`"discover-1"`, "server/discover", "")
	if r.status != 200 || r.members["result"] == nil {
		f.t.Fatalf("server/discover: %d %s", r.status, r.body)
	}
	return r
}

type toolAnswer struct {
	reply
	text       string // the single text block
	structured json.RawMessage
	isError    bool
}

// call is tools/call. It requires a tool result (not a JSON-RPC error) of the documented shape.
func (f *fixture) call(id, name, arguments string) toolAnswer {
	f.t.Helper()
	r := f.rpc(id, "tools/call", `"name":"`+name+`","arguments":`+arguments)
	if r.status != 200 || r.members["result"] == nil {
		f.t.Fatalf("tools/call %s: %d %s", name, r.status, r.body)
	}
	var result struct {
		Content []struct {
			Type string
			Text *string
		}
		StructuredContent json.RawMessage
		IsError           *bool
	}
	if err := json.Unmarshal(r.members["result"], &result); err != nil {
		f.t.Fatalf("tools/call %s: result %s", name, r.members["result"])
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" || result.Content[0].Text == nil || result.IsError == nil {
		f.t.Fatalf("tools/call %s: result %s", name, r.members["result"])
	}
	a := toolAnswer{reply: r, text: *result.Content[0].Text, structured: result.StructuredContent, isError: *result.IsError}
	if a.isError && a.structured != nil {
		f.t.Fatalf("tools/call %s: an error result carries structuredContent: %s", name, r.body)
	}
	if a.structured != nil && a.text != string(a.structured) {
		f.t.Fatalf("tools/call %s: text and structuredContent differ:\n%s\n%s", name, a.text, a.structured)
	}
	return a
}

// ok is call for a call that must succeed.
func (f *fixture) ok(id, name, arguments string) toolAnswer {
	f.t.Helper()
	a := f.call(id, name, arguments)
	if a.isError {
		f.t.Fatalf("tools/call %s failed: %s", name, a.text)
	}
	return a
}

func (f *fixture) repo(name string) *store.Repo {
	f.t.Helper()
	r, err := f.st.CreateRepo(store.NewRepo{Name: name, Private: true, Description: "github-core-clone-01 MCP setup", HasDescription: true})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// pullSetup is the suite's preparePullRequest: a first commit on main through the contents API on
// the empty repository, a head branch at that commit, and one more commit on the head branch.
func (f *fixture) pullSetup(part string) (r *store.Repo, head string) {
	f.t.Helper()
	r = f.repo("mcp-" + tag + "-" + part)
	base, err := r.PutFile(store.FileWrite{Path: "mcp-" + tag + "-" + part + "-base.txt", Message: "Fix repo lookup", Data: []byte("base content\n")})
	if err != nil {
		f.t.Fatal(err)
	}
	head = "mcp-head-" + tag + "-" + part
	if err := r.CreateRef("refs/heads/"+head, base.Commit.ID); err != nil {
		f.t.Fatal(err)
	}
	if _, err := r.PutFile(store.FileWrite{Path: "mcp-" + tag + "-" + part + "-head.txt", Message: "Add clone test\n\nCovers the empty repository.", Data: []byte("head content\n"), Branch: head}); err != nil {
		f.t.Fatal(err)
	}
	return r, head
}

func field[T any](t *testing.T, object []byte, key string) T {
	t.Helper()
	var members map[string]json.RawMessage
	var v T
	if err := json.Unmarshal(object, &members); err != nil {
		t.Fatalf("not a JSON object: %s", object)
	}
	if err := json.Unmarshal(members[key], &v); err != nil {
		t.Fatalf("member %q of %s: %v", key, object, err)
	}
	return v
}

// ---------------------------------------------------------------- the 15 checks

// Checks 1 (mcp.initialize) and 2 (mcp.server-discover): the discover result names the server,
// declares the tools capability as an object and lists revisions as dates.
func TestCheckDiscover(t *testing.T) {
	f := newFixture(t)
	r := f.connect()
	var result struct {
		SupportedVersions []string
		Capabilities      map[string]json.RawMessage
		ServerInfo        map[string]string
		Meta              map[string]map[string]string `json:"_meta"`
	}
	if err := json.Unmarshal(r.members["result"], &result); err != nil {
		t.Fatal(err)
	}
	if len(result.SupportedVersions) == 0 || result.SupportedVersions[0] != "2026-07-28" {
		t.Fatalf("supportedVersions %v", result.SupportedVersions)
	}
	for _, v := range result.SupportedVersions {
		if _, err := time.Parse("2006-01-02", v); err != nil || len(v) != 10 {
			t.Errorf("revision %q is not YYYY-MM-DD", v)
		}
	}
	if tools := result.Capabilities["tools"]; len(tools) == 0 || tools[0] != '{' {
		t.Errorf("capabilities.tools is not an object: %s", tools)
	}
	// The client prefers the _meta copy, so both must be complete and the same.
	for _, info := range []map[string]string{result.ServerInfo, result.Meta["io.modelcontextprotocol/serverInfo"]} {
		if strings.TrimSpace(info["name"]) == "" || strings.TrimSpace(info["version"]) == "" || len(info) != 2 {
			t.Errorf("server identity %v", info)
		}
	}

	// Nothing optional is validated: no protocol headers, no params, another revision in _meta.
	bare := "POST /mcp HTTP/1.1\r\nHost: " + testHost + "\r\nAuthorization: bearer " + testToken + "\r\nContent-Length: "
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":"discover-1","method":"server/discover"}`,
		`{"id":7,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01"}}}`,
		" \r\n\t" + `{"jsonrpc":"2.0","id":"discover-1","method":"server/discover","params":null}` + "\n",
	} {
		r := parseReply(t, f.tester.Do([]byte(bare+strconv.Itoa(len(body))+"\r\n\r\n"+body)))
		if r.status != 200 || r.members["result"] == nil {
			t.Errorf("lenient discover %s: %d %s", body, r.status, r.body)
		}
	}
}

// The handshake of the revisions before 2026-07-28 (the suite's fallback path): initialize answers
// the offered revision, the notification gets 202 and no body, and neither gates anything.
func TestLegacyHandshake(t *testing.T) {
	f := newFixture(t)
	for offered, want := range map[string]string{"2025-11-25": "2025-11-25", "2025-06-18": "2025-06-18", "2025-03-26": "2025-03-26", "2024-11-05": "2024-11-05", "2026-07-28": "2025-11-25", "1999-01-01": "2025-11-25"} {
		r := f.rpc(`"initialize-1"`, "initialize", `"protocolVersion":"`+offered+`","capabilities":{},"clientInfo":{"name":"makefun-github-core-clone-env","version":"1.0.0"}`)
		result := r.members["result"]
		if r.status != 200 || result == nil || field[string](t, result, "protocolVersion") != want {
			t.Fatalf("initialize %s: %d %s", offered, r.status, r.body)
		}
		info := field[map[string]string](t, result, "serverInfo")
		if info["name"] == "" || info["version"] == "" || string(field[map[string]json.RawMessage](t, result, "capabilities")["tools"]) != `{"listChanged":false}` {
			t.Errorf("initialize result %s", result)
		}
	}
	if r := f.post("initialize", `{"jsonrpc":"2.0","id":"initialize-1","method":"initialize"}`); r.status != 200 || r.members["result"] == nil {
		t.Errorf("initialize without params: %d %s", r.status, r.body)
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{` + meta + `}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled"}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"create_repo","arguments":{"name":"from-a-notification"}}}`,
	} {
		raw := f.tester.Do(request("Bearer "+testToken, "notifications/initialized", body))
		r := parseReply(t, raw)
		if r.status != 202 || len(r.body) != 0 || r.header["content-length"] != "0" {
			t.Errorf("notification %s: %q", body, raw)
		}
	}
	if len(f.st.Repos()) != 0 {
		t.Error("a notification ran a tool")
	}
	if r := f.rpc("1", "ping", ""); string(r.members["result"]) != "{}" {
		t.Errorf("ping: %s", r.body)
	}
}

// specTools is the tools array of spec/MCP.md section 5.1, verbatim.
const specTools = `[
  {
    "name": "create_repo",
    "description": "Create a new repository owned by the authenticated user. Returns the repository object, as GET /api/v3/repos/{owner}/{repo} returns it.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "name": {"type": "string", "description": "Repository name"},
        "description": {"type": "string", "description": "Repository description"},
        "private": {"type": "boolean", "description": "Whether the repository is private"}
      },
      "required": ["name"]
    }
  },
  {
    "name": "get_repo",
    "description": "Get a repository by owner and name. Returns the repository object.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "owner": {"type": "string", "description": "Repository owner (user login)"},
        "repo": {"type": "string", "description": "Repository name"}
      },
      "required": ["owner", "repo"]
    }
  },
  {
    "name": "create_issue",
    "description": "Open a new issue in a repository. Returns the issue object, as GET /api/v3/repos/{owner}/{repo}/issues/{number} returns it.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "owner": {"type": "string", "description": "Repository owner (user login)"},
        "repo": {"type": "string", "description": "Repository name"},
        "title": {"type": "string", "description": "Issue title"},
        "body": {"type": "string", "description": "Issue body"}
      },
      "required": ["owner", "repo", "title"]
    }
  },
  {
    "name": "list_issues",
    "description": "List the issues of a repository. Returns a JSON array of issue objects.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "owner": {"type": "string", "description": "Repository owner (user login)"},
        "repo": {"type": "string", "description": "Repository name"},
        "state": {"type": "string", "enum": ["open", "closed", "all"], "description": "Filter by state (default open)"}
      },
      "required": ["owner", "repo"]
    }
  },
  {
    "name": "create_pull_request",
    "description": "Open a pull request that merges the head branch into the base branch. Returns the pull request object.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "owner": {"type": "string", "description": "Repository owner (user login)"},
        "repo": {"type": "string", "description": "Repository name"},
        "title": {"type": "string", "description": "Pull request title"},
        "body": {"type": "string", "description": "Pull request description"},
        "head": {"type": "string", "description": "Branch that holds the changes"},
        "base": {"type": "string", "description": "Branch to merge into"}
      },
      "required": ["owner", "repo", "title", "head", "base"]
    }
  },
  {
    "name": "merge_pull_request",
    "description": "Merge an open pull request with a squash commit on its base branch. Returns {sha, merged, message}.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "owner": {"type": "string", "description": "Repository owner (user login)"},
        "repo": {"type": "string", "description": "Repository name"},
        "pull_number": {"type": "number", "description": "Pull request number"}
      },
      "required": ["owner", "repo", "pull_number"]
    }
  }
]`

type listedTool struct {
	Name        string
	Description string
	InputSchema struct {
		Type       string
		Properties map[string]json.RawMessage
		Required   []string
	}
}

func (f *fixture) listTools(id string) (raw json.RawMessage, tools []listedTool) {
	f.t.Helper()
	r := f.rpc(id, "tools/list", "")
	if r.status != 200 || r.members["result"] == nil {
		f.t.Fatalf("tools/list: %d %s", r.status, r.body)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(r.members["result"], &result); err != nil || len(result) != 1 {
		f.t.Fatalf("tools/list result (no nextCursor, nothing but tools): %s", r.members["result"])
	}
	if err := json.Unmarshal(result["tools"], &tools); err != nil {
		f.t.Fatal(err)
	}
	return result["tools"], tools
}

// Checks 3 (mcp.tools-list), 4 (mcp.tool-schemas) and 13 (mcp.tools-list-idempotent).
func TestCheckToolsList(t *testing.T) {
	f := newFixture(t)
	f.connect()
	first, tools := f.listTools("1")
	second, _ := f.listTools("2")
	if !bytes.Equal(first, second) {
		t.Fatal("two tools/list answers differ")
	}
	// Tool calls in between change nothing either.
	f.ok("3", "create_repo", `{"name":"between-lists"}`)
	if third, _ := f.listTools("4"); !bytes.Equal(first, third) {
		t.Fatal("tools/list changed after a tool call")
	}

	var want bytes.Buffer
	if err := json.Compact(&want, []byte(specTools)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, want.Bytes()) {
		t.Fatalf("tools differ from the spec:\n got %s\nwant %s", first, want.Bytes())
	}

	names := []string{"create_repo", "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request"}
	if len(tools) != len(names) {
		t.Fatalf("%d tools", len(tools))
	}
	for i, tool := range tools {
		if tool.Name != names[i] || strings.TrimSpace(tool.Description) == "" || tool.InputSchema.Type != "object" {
			t.Errorf("tool %d: %+v", i, tool)
		}
		if findTool(tool.Name) == nil || findTool(tool.Name).run == nil {
			t.Errorf("tool %s is listed but cannot be called", tool.Name)
		}
	}
}

// The suite does not send our property names: it maps its own argument names onto each schema by
// trying synonyms in order (checks-mcp.ts mapArguments). This is that mapping, to prove that every
// call the suite makes reaches the property it means and fills every required one.
var argumentSynonyms = map[string][]string{
	"owner":       {"owner", "owner_login", "org", "organization", "organisation", "user", "user_login", "login", "account", "namespace", "owner_name"},
	"repo":        {"repo", "repository", "repository_name", "repo_name", "repo_slug", "project", "project_name"},
	"title":       {"title", "issue_title", "pr_title", "pull_request_title", "subject"},
	"body":        {"body", "issue_body", "description", "content", "text", "comment"},
	"head":        {"head", "head_ref", "head_branch", "pull_request_head", "pr_head", "source", "source_ref", "source_branch", "from", "from_branch", "branch"},
	"base":        {"base", "base_ref", "base_branch", "pull_request_base", "pr_base", "target", "target_ref", "target_branch", "into", "into_branch"},
	"pull_number": {"pull_number", "pull_request_number", "pr_number", "number", "issue_number", "index"},
	"name":        {"name", "repo", "repository", "repository_name", "repo_name"},
	"description": {"description", "about", "summary", "body"},
	"private":     {"private", "is_private"},
	"state":       {"state", "status"},
}

var fullNameSynonyms = []string{"full_name", "repo_full_name", "repository_full_name", "fullName"}

func normalize(name string) string {
	var b []byte
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if ch >= 'A' && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			b = append(b, ch)
		}
	}
	return string(b)
}

func TestSuiteArgumentMapping(t *testing.T) {
	f := newFixture(t)
	_, tools := f.listTools("1")
	byName := map[string]listedTool{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	cases := []struct {
		tool     string
		semantic []string // the suite's argument names for this call
		want     map[string]string
	}{
		{"create_repo", []string{"owner", "name", "description", "private"}, map[string]string{"name": "name", "description": "description", "private": "private"}},
		{"get_repo", []string{"owner", "repo"}, map[string]string{"owner": "owner", "repo": "repo"}},
		{"create_issue", []string{"owner", "repo", "title", "body"}, map[string]string{"owner": "owner", "repo": "repo", "title": "title", "body": "body"}},
		{"list_issues", []string{"owner", "repo", "state"}, map[string]string{"owner": "owner", "repo": "repo", "state": "state"}},
		{"create_pull_request", []string{"owner", "repo", "title", "body", "head", "base"}, map[string]string{"owner": "owner", "repo": "repo", "title": "title", "body": "body", "head": "head", "base": "base"}},
		{"merge_pull_request", []string{"owner", "repo", "pull_number"}, map[string]string{"owner": "owner", "repo": "repo", "pull_number": "pull_number"}},
	}
	for _, tc := range cases {
		tool := byName[tc.tool]
		normalized := map[string]string{}
		for property := range tool.InputSchema.Properties {
			normalized[normalize(property)] = property
		}
		propertyFor := func(synonyms []string) string {
			for _, synonym := range synonyms {
				if property, ok := normalized[normalize(synonym)]; ok {
					return property
				}
			}
			return ""
		}
		got := map[string]string{} // property -> the suite argument that fills it
		for _, key := range tc.semantic {
			if property := propertyFor(argumentSynonyms[key]); property != "" {
				if other, taken := got[property]; taken {
					t.Errorf("%s: property %s is filled by both %s and %s", tc.tool, property, other, key)
				}
				got[property] = key
			}
		}
		if propertyFor(fullNameSynonyms) != "" {
			t.Errorf("%s: a full-name property would be filled as well", tc.tool)
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: mapped %v, want %v", tc.tool, got, tc.want)
		}
		for property, key := range tc.want {
			if got[property] != key {
				t.Errorf("%s: property %s gets %q, want %q", tc.tool, property, got[property], key)
			}
		}
		for _, required := range tool.InputSchema.Required {
			if _, ok := got[required]; !ok {
				t.Errorf("%s: required %s gets no value from the suite", tc.tool, required)
			}
		}
		if strings.Contains(string(mustJSON(t, tool.InputSchema.Properties)), "x-mcp-header") {
			t.Errorf("%s: x-mcp-header would make the client mirror arguments into headers", tc.tool)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Check 5 (mcp.create-issue): the issue made by the tool is the issue REST lists.
func TestCheckCreateIssue(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.listTools("1")
	r := f.repo("mcp-" + tag + "-issue-create")
	a := f.ok("2", "create_issue", `{"owner":"`+owner+`","repo":"`+r.Name+`","title":"Broken clone over HTTP","body":"Steps to reproduce: clone twice."}`)

	if len(r.Issues) != 1 {
		t.Fatalf("%d issues in the store", len(r.Issues))
	}
	is := r.Issues[0]
	if is.Title != "Broken clone over HTTP" || is.Body != "Steps to reproduce: clone twice." || is.Closed || is.Number != 1 {
		t.Fatalf("stored issue %+v", is)
	}
	// What GET .../issues/1 and the list serve for this host.
	if want := render.AppendIssue(nil, []byte(testHost), is); a.text != string(want) {
		t.Fatalf("tool answer differs from the REST rendering:\n%s\n%s", a.text, want)
	}
	if field[int64](t, a.structured, "number") != 1 || field[int64](t, a.structured, "id") != is.ID || field[string](t, a.structured, "state") != "open" {
		t.Errorf("issue object %s", a.structured)
	}
	if url := field[string](t, a.structured, "html_url"); url != "http://"+testHost+"/"+owner+"/"+r.Name+"/issues/1" {
		t.Errorf("html_url %s", url)
	}

	// Text is stored and returned byte for byte, whatever it contains.
	title := "quotes \" and \\ and <b>&amp;</b> \u00e9\u4e16\U0001F600"
	body := "line one\nline two\ttabbed\r\n"
	a = f.ok("3", "create_issue", `{"owner":"`+strings.ToUpper(owner)+`","repo":"`+r.Name+`","title":`+string(mustJSON(t, title))+`,"body":`+string(mustJSON(t, body))+`,"labels":["ignored"],"unknown":{"a":1}}`)
	if got := r.Issues[1]; got.Title != title || got.Body != body {
		t.Errorf("stored %q / %q", got.Title, got.Body)
	}
	if field[string](t, a.structured, "title") != title || field[string](t, a.structured, "body") != body {
		t.Errorf("returned %s", a.structured)
	}
	// No body: an empty string, never null.
	a = f.ok("4", "create_issue", `{"owner":"`+owner+`","repo":"`+r.Name+`","title":"No body","body":null}`)
	if !strings.Contains(a.text, `"body":""`) {
		t.Errorf("issue without a body: %s", a.text)
	}
}

// Check 6 (mcp.list-issues): an issue made over REST shows up, by title and by number.
func TestCheckListIssues(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.listTools("1")
	r := f.repo("mcp-" + tag + "-issue-list")
	is, err := r.CreateIssue(store.NewIssue{Title: "Slow push with large blobs", Body: "Seen on every third push."})
	if err != nil {
		t.Fatal(err)
	}
	args := `"owner":"` + owner + `","repo":"` + r.Name + `"`
	a := f.ok("2", "list_issues", `{`+args+`,"state":"open"}`)
	if a.structured != nil {
		t.Errorf("a list result carries structuredContent: %s", a.body)
	}
	if !strings.Contains(a.text, is.Title) || !strings.Contains(a.text, `"number":1,`) {
		t.Fatalf("list_issues: %s", a.text)
	}
	if want := render.AppendIssues(nil, []byte(testHost), []*store.Issue{is}); a.text != string(want) {
		t.Fatalf("list differs from the REST rendering:\n%s\n%s", a.text, want)
	}

	// States, newest first, pull requests left out, and the default.
	closed, _ := r.CreateIssue(store.NewIssue{Title: "Closed one"})
	state := "closed"
	if err := r.UpdateIssue(closed, store.IssuePatch{State: &state}); err != nil {
		t.Fatal(err)
	}
	third, _ := r.CreateIssue(store.NewIssue{Title: "Third"})
	numbers := func(a toolAnswer) (out []int64) {
		var list []struct{ Number int64 }
		if err := json.Unmarshal([]byte(a.text), &list); err != nil {
			t.Fatalf("list text is not a JSON array: %s", a.text)
		}
		for _, item := range list {
			out = append(out, item.Number)
		}
		return out
	}
	for arguments, want := range map[string][]int64{
		`{` + args + `}`:                  {third.Number, is.Number},
		`{` + args + `,"state":null}`:     {third.Number, is.Number},
		`{` + args + `,"state":"OPEN"}`:   {third.Number, is.Number},
		`{` + args + `,"state":"closed"}`: {closed.Number},
		`{` + args + `,"state":"all"}`:    {third.Number, closed.Number, is.Number},
	} {
		if got := numbers(f.ok("3", "list_issues", arguments)); !equalInts(got, want) {
			t.Errorf("list_issues %s: %v, want %v", arguments, got, want)
		}
	}
	if a := f.call("4", "list_issues", `{`+args+`,"state":"merged"}`); !a.isError || !strings.Contains(a.text, "state") {
		t.Errorf("unknown state: %s", a.body)
	}
	empty := f.repo("no-issues-yet")
	if a := f.ok("5", "list_issues", `{"owner":"`+owner+`","repo":"`+empty.Name+`"}`); a.text != "[]" {
		t.Errorf("empty list: %s", a.text)
	}
}

func equalInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Check 7 (mcp.create-pull-request): REST reads the pull request back with the same head and base.
func TestCheckCreatePullRequest(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.listTools("1")
	r, head := f.pullSetup("pr-create")
	args := `"owner":"` + owner + `","repo":"` + r.Name + `","title":"Add clone retries","body":"Retries the clone once.","head":"` + head + `","base":"main"`
	a := f.ok("2", "create_pull_request", `{`+args+`}`)

	number := field[int64](t, a.structured, "number")
	p := r.Pull(number)
	if p == nil || p.HeadRef != head || p.BaseRef != "main" || p.Title != "Add clone retries" || p.Body != "Retries the clone once." || p.Closed {
		t.Fatalf("stored pull request %+v", p)
	}
	// What GET .../pulls/{number} serves.
	if want := render.AppendPull(nil, []byte(testHost), p); a.text != string(want) {
		t.Fatalf("tool answer differs from the REST rendering:\n%s\n%s", a.text, want)
	}
	for side, ref := range map[string]string{"head": head, "base": "main"} {
		if got := field[string](t, field[json.RawMessage](t, a.structured, side), "ref"); got != ref {
			t.Errorf("%s.ref %q", side, got)
		}
	}
	if field[bool](t, a.structured, "merged") || field[string](t, a.structured, "state") != "open" {
		t.Errorf("pull request %s", a.structured)
	}

	// Refusals are tool errors in REST's words, and nothing is created.
	for arguments, want := range map[string]string{
		`{` + args + `}`: "A pull request already exists",
		`{"owner":"` + owner + `","repo":"` + r.Name + `","title":"t","head":"no-such-branch","base":"main"}`: "Validation Failed",
		`{"owner":"` + owner + `","repo":"` + r.Name + `","title":"t","head":"` + head + `","base":"nope"}`:   "Validation Failed",
		`{"owner":"` + owner + `","repo":"nope","title":"t","head":"` + head + `","base":"main"}`:             "Not Found",
		`{"owner":"` + owner + `","repo":"` + r.Name + `","title":"t","head":"` + head + `"}`:                 "missing required argument: base",
	} {
		if a := f.call("3", "create_pull_request", arguments); !a.isError || !strings.Contains(a.text, want) {
			t.Errorf("create_pull_request %s: %s", arguments, a.body)
		}
	}
	if len(r.Pulls) != 1 {
		t.Errorf("%d pull requests after refused calls", len(r.Pulls))
	}
	// The owner-qualified head of the REST API works too.
	r2, head2 := f.pullSetup("pr-create-2")
	a = f.ok("4", "create_pull_request", `{"owner":"`+owner+`","repo":"`+r2.Name+`","title":"Qualified","head":"`+owner+`:`+head2+`","base":"main"}`)
	if got := field[string](t, field[json.RawMessage](t, a.structured, "head"), "ref"); got != head2 {
		t.Errorf("head.ref %q", got)
	}
}

// Check 8 (mcp.merge-pull-request): after the tool, REST reads merged true.
func TestCheckMergePullRequest(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.listTools("1")
	r, head := f.pullSetup("pr-merge")
	p, err := r.CreatePull(store.NewPull{Title: "Fix ref advertisement", Head: head, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	args := `"owner":"` + owner + `","repo":"` + r.Name + `"`
	a := f.ok("2", "merge_pull_request", `{`+args+`,"pull_number":`+strconv.FormatInt(p.Number, 10)+`}`)

	if !p.Merged || !p.Closed {
		t.Fatalf("pull request after the merge: %+v", p)
	}
	tip, _ := r.Branch("main")
	if a.text != `{"sha":"`+tip.String()+`","merged":true,"message":"Pull Request successfully merged"}` {
		t.Fatalf("merge result %s (main is at %s)", a.text, tip)
	}
	commit := r.Objects.GetCommit(tip)
	if commit == nil || len(commit.Commit.Parents) != 1 {
		t.Fatalf("the merge is not a squash commit: %+v", commit)
	}
	if _, err := r.Contents("mcp-"+tag+"-pr-merge-head.txt", "main"); err != nil {
		t.Errorf("the head's file is not on main: %v", err)
	}
	rendered := render.AppendPull(nil, []byte(testHost), p)
	if !field[bool](t, rendered, "merged") || field[string](t, rendered, "state") != "closed" {
		t.Errorf("REST rendering after the merge: %s", rendered)
	}

	// Merging again, an unknown number, an issue's number: tool errors.
	is, _ := r.CreateIssue(store.NewIssue{Title: "An issue, not a pull request"})
	for arguments, want := range map[string]string{
		`{` + args + `,"pull_number":` + strconv.FormatInt(p.Number, 10) + `}`:  store.MsgNotMergeable,
		`{` + args + `,"pull_number":99}`:                                       "Not Found",
		`{` + args + `,"pull_number":` + strconv.FormatInt(is.Number, 10) + `}`: "Not Found",
		`{` + args + `,"pull_number":0}`:                                        "pull_number must be a positive integer",
		`{` + args + `,"pull_number":1.5}`:                                      "pull_number must be a positive integer",
		`{` + args + `,"pull_number":-1}`:                                       "pull_number must be a positive integer",
		`{` + args + `,"pull_number":"abc"}`:                                    "pull_number must be a positive integer",
		`{` + args + `,"pull_number":true}`:                                     "pull_number must be a positive integer",
		`{` + args + `}`:                                                        "missing required argument: pull_number",
	} {
		if a := f.call("3", "merge_pull_request", arguments); !a.isError || !strings.Contains(a.text, want) {
			t.Errorf("merge_pull_request %s: %s", arguments, a.body)
		}
	}

	// The number may arrive as 2, 2.0, 2e0 or "2".
	for i, spelling := range []string{`%d`, `%d.0`, `%de0`, `"%d"`} {
		r, head := f.pullSetup("pr-merge-n" + strconv.Itoa(i))
		p, err := r.CreatePull(store.NewPull{Title: "Spelled " + spelling, Head: head, Base: "main"})
		if err != nil {
			t.Fatal(err)
		}
		number := strings.Replace(spelling, "%d", strconv.FormatInt(p.Number, 10), 1)
		f.ok("4", "merge_pull_request", `{"owner":"`+owner+`","repo":"`+r.Name+`","pull_number":`+number+`}`)
		if !p.Merged {
			t.Errorf("pull_number %s did not merge", number)
		}
	}
}

// Check 9 (mcp.unknown-tool): a JSON-RPC error -32602 with HTTP 200, not a tool result.
func TestCheckUnknownTool(t *testing.T) {
	f := newFixture(t)
	f.connect()
	r := f.rpc("1", "tools/call", `"name":"mcp_no_such_tool_`+tag+`","arguments":{}`)
	if r.status != 200 || r.code != codeInvalidParams || r.message != "Unknown tool: mcp_no_such_tool_"+tag {
		t.Fatalf("unknown tool: %d %s", r.status, r.body)
	}
	for _, params := range []string{`"arguments":{}`, `"name":"","arguments":{}`, `"name":7`, `"name":"create_issue","arguments":[]`, `"name":"create_issue","arguments":"x"`, `"name":"Create_Issue"`} {
		if r := f.rpc("2", "tools/call", params); r.status != 200 || r.code != codeInvalidParams {
			t.Errorf("tools/call {%s}: %d %s", params, r.status, r.body)
		}
	}
	if r := f.post("tools/call", `{"jsonrpc":"2.0","id":3,"method":"tools/call"}`); r.status != 200 || r.code != codeInvalidParams {
		t.Errorf("tools/call without params: %d %s", r.status, r.body)
	}
	if r := f.rpc("4", "resources/list", ""); r.status != 200 || r.code != codeMethodNotFound || r.message != "Method not found: resources/list" {
		t.Errorf("unknown method: %d %s", r.status, r.body)
	}
}

// Check 10 (mcp.parse-error): HTTP 400, -32700, a JSON body with an "id": null member.
func TestCheckParseError(t *testing.T) {
	f := newFixture(t)
	f.connect()
	for _, body := range []string{`{"jsonrpc": "2.0", "id": 424242, "method": "tools/list"`, ``, `   `, `not json`, `{"id":1,"method":"ping"}{}`, `{'id':1}`} {
		r := f.post("tools/list", body)
		if r.status != 400 || r.code != codeParseError || string(r.body) != `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}` {
			t.Errorf("body %q: %d %s", body, r.status, r.body)
		}
	}
	// Valid JSON that is not a request: -32600, with the id when it has a usable one.
	for body, id := range map[string]string{
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`: `null`,
		`42`:                                    `null`,
		`"ping"`:                                `null`,
		`null`:                                  `null`,
		`{}`:                                    `null`,
		`{"jsonrpc":"2.0","id":5}`:              `5`,
		`{"jsonrpc":"2.0","id":"a","method":7}`: `"a"`,
		`{"jsonrpc":"2.0","id":6,"method":""}`:  `6`,
		`{"jsonrpc":"2.0","id":6,"result":{}}`:  `6`,
		`{"jsonrpc":"2.0","id":{"x":1},"method":"ping"}`: `null`,
		`{"jsonrpc":"2.0","id":[1],"method":"ping"}`:     `null`,
		`{"jsonrpc":"2.0","id":true,"method":"ping"}`:    `null`,
	} {
		r := f.post("ping", body)
		if r.status != 400 || r.code != codeInvalidRequest || string(r.members["id"]) != id {
			t.Errorf("body %s: %d %s", body, r.status, r.body)
		}
	}
}

// Check 11 (mcp.missing-arguments): isError true, and nothing happens.
func TestCheckMissingArguments(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.listTools("1")
	r := f.repo("target")
	a := f.call("2", "create_issue", `{}`)
	if !a.isError || a.text != "missing required argument: owner" {
		t.Fatalf("create_issue {}: %s", a.body)
	}
	args := `"owner":"` + owner + `","repo":"` + r.Name + `"`
	for arguments, want := range map[string]string{
		`null`:                                                "missing required argument: owner",
		`{"owner":"` + owner + `"}`:                           "missing required argument: repo",
		`{` + args + `}`:                                      "missing required argument: title",
		`{` + args + `,"title":null}`:                         "missing required argument: title",
		`{` + args + `,"title":""}`:                           "missing required argument: title",
		`{` + args + `,"title":7}`:                            "invalid argument: title must be a string",
		`{` + args + `,"title":["x"]}`:                        "invalid argument: title must be a string",
		`{` + args + `,"title":"t","body":{"text":"x"}}`:      "invalid argument: body must be a string",
		`{"owner":"someone-else","repo":"x","title":"t"}`:     "Not Found",
		`{"owner":"` + owner + `","repo":"gone","title":"t"}`: "Not Found",
	} {
		if a := f.call("3", "create_issue", arguments); !a.isError || !strings.Contains(a.text, want) {
			t.Errorf("create_issue %s: %s", arguments, a.body)
		}
	}
	if r := f.post("tools/call", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"create_issue"}}`); !bytes.Contains(r.body, []byte(`"isError":true`)) {
		t.Errorf("create_issue without an arguments member: %s", r.body)
	}
	if len(r.Issues) != 0 {
		t.Fatalf("a refused call created %d issues", len(r.Issues))
	}
	for name, want := range map[string]string{"create_repo": "name", "get_repo": "owner", "list_issues": "owner", "create_pull_request": "owner", "merge_pull_request": "owner"} {
		if a := f.call("5", name, `{}`); !a.isError || a.text != "missing required argument: "+want {
			t.Errorf("%s {}: %s", name, a.body)
		}
	}
	if len(f.st.Repos()) != 1 {
		t.Error("a refused create_repo created a repository")
	}
}

// socketClient is one TCP connection to a live server.
type socketClient struct {
	conn net.Conn
	r    *bufio.Reader
}

func dial(t *testing.T, port int) *socketClient {
	t.Helper()
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	return &socketClient{conn: conn, r: bufio.NewReader(conn)}
}

// read reads one response and returns its body.
func (c *socketClient) read() ([]byte, error) {
	length := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" {
			break
		}
		if name, value, ok := strings.Cut(strings.TrimSuffix(line, "\r\n"), ": "); ok && strings.EqualFold(name, "Content-Length") {
			length, _ = strconv.Atoi(value)
		}
	}
	if length < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, length)
	_, err := io.ReadFull(c.r, body)
	return body, err
}

func getRepoCall(id, repo string) []byte {
	return request("Bearer "+testToken, "tools/call", `{"jsonrpc":"2.0","id":`+id+`,"method":"tools/call","params":{"name":"get_repo","arguments":{"owner":"`+owner+`","repo":"`+repo+`"},`+meta+`}}`)
}

// Check 12 (mcp.concurrent-calls): two calls in flight on two connections each get their own id,
// as the string it was, and their own repository.
func TestCheckConcurrentCalls(t *testing.T) {
	f := newFixture(t)
	ln, err := httpd.Listen()
	if err != nil {
		t.Fatal(err)
	}
	go f.srv.Serve(ln)
	t.Cleanup(func() { ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	f.st.Lock()
	left, right := f.repo("mcp-"+tag+"-concurrent-left"), f.repo("mcp-"+tag+"-concurrent-right")
	f.st.Unlock()

	// Both requests are on the wire before either answer is read.
	a, b := dial(t, port), dial(t, port)
	leftID, rightID := `"concurrent-left-`+tag+`"`, `"concurrent-right-`+tag+`"`
	a.conn.Write(getRepoCall(leftID, left.Name))
	b.conn.Write(getRepoCall(rightID, right.Name))
	for _, side := range []struct {
		c        *socketClient
		id, repo string
	}{{b, rightID, right.Name}, {a, leftID, left.Name}} {
		body, err := side.c.read()
		if err != nil {
			t.Fatal(err)
		}
		if string(field[json.RawMessage](t, body, "id")) != side.id || !bytes.Contains(body, []byte(`\"name\":\"`+side.repo+`\"`)) || bytes.Contains(body, []byte(`"error"`)) {
			t.Fatalf("answer to %s: %s", side.id, body)
		}
	}

	// The same under load: many connections, every answer checked against its own request.
	const clients, rounds = 8, 150
	var wg sync.WaitGroup
	failures := make(chan string, clients)
	for n := 0; n < clients; n++ {
		c := dial(t, port)
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				repo, id := left.Name, `"left-`+strconv.Itoa(n)+`-`+strconv.Itoa(i)+`"`
				if (n+i)%2 == 1 {
					repo, id = right.Name, strconv.Itoa(n*1000+i) // a numeric id stays a number
				}
				if _, err := c.conn.Write(getRepoCall(id, repo)); err != nil {
					failures <- err.Error()
					return
				}
				body, err := c.read()
				if err != nil {
					failures <- err.Error()
					return
				}
				if !bytes.HasPrefix(body, []byte(`{"jsonrpc":"2.0","id":`+id+`,"result":`)) || !bytes.Contains(body, []byte(`"full_name":"`+owner+`/`+repo+`"`)) {
					failures <- "answer to " + id + ": " + string(body)
					return
				}
			}
		}(n)
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}

// Ids come back exactly as they were sent.
func TestIDsAreEchoedRaw(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{`1`, `0`, `424242`, `-7`, `1.5`, `1e3`, `"discover-1"`, `""`, `"1"`, `"with \"quotes\" and \\ and \u00e9"`, `null`, `9007199254740993`} {
		r := f.post("ping", `{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`)
		if r.status != 200 || string(r.body) != `{"jsonrpc":"2.0","id":`+id+`,"result":{}}` {
			t.Errorf("id %s: %d %s", id, r.status, r.body)
		}
	}
	r := f.post("ping", `{ "method" : "ping", "id" :   "spaced"  , "jsonrpc" : "2.0" }`)
	if string(r.body) != `{"jsonrpc":"2.0","id":"spaced","result":{}}` {
		t.Errorf("spaced id: %s", r.body)
	}
	// A string id that is not UTF-8 cannot be echoed into a JSON reply.
	r = f.post("ping", `{"jsonrpc":"2.0","id":"bad `+"\xff"+`","method":"ping"}`)
	if r.status != 400 || r.code != codeInvalidRequest || string(r.members["id"]) != "null" {
		t.Errorf("id that is not UTF-8: %d %s", r.status, r.body)
	}
}

// Check 14 (mcp.rest-parity): the repository and the issue made over MCP are the store's objects,
// rendered as REST renders them, with ids that stay the same.
func TestCheckRestParity(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.listTools("1")
	name := "mcp-" + tag + "-parity"
	a := f.ok("2", "create_repo", `{"name":"`+name+`","description":"notes and experiments, kept until they are not","private":true}`)

	if got := field[string](t, a.structured, "full_name"); got != owner+"/"+name {
		t.Fatalf("full_name %q", got)
	}
	r := f.st.Lookup([]byte(owner), []byte(name))
	if r == nil {
		t.Fatal("REST cannot find the repository")
	}
	if !r.Private || r.Description != "notes and experiments, kept until they are not" || !r.HasDescription || r.DefaultBranch != "main" || !r.IsEmpty() {
		t.Errorf("stored repository %+v", r)
	}
	rest := render.AppendRepo(nil, []byte(testHost), r)
	if a.text != string(rest) {
		t.Fatalf("create_repo differs from GET /repos/%s/%s:\n%s\n%s", owner, name, a.text, rest)
	}
	if field[int64](t, rest, "id") != r.ID || field[int64](t, a.structured, "id") != r.ID {
		t.Errorf("repository id: %s", a.structured)
	}
	if got := f.ok("3", "get_repo", `{"owner":"`+owner+`","repo":"`+name+`"}`); got.text != string(rest) {
		t.Errorf("get_repo differs from create_repo:\n%s\n%s", got.text, rest)
	}

	issue := f.ok("4", "create_issue", `{"owner":"`+owner+`","repo":"`+name+`","title":"Wrong default branch","body":"HEAD points at a branch that does not exist."}`)
	number := field[int64](t, issue.structured, "number")
	is := r.Issue(number)
	if is == nil || is.Title != "Wrong default branch" {
		t.Fatalf("REST cannot read issue %d: %+v", number, is)
	}
	for read := 0; read < 2; read++ {
		rest := httpd.TailBody(render.IssueTail([]byte(testHost), is)) // the bytes GET .../issues/{n} sends
		if issue.text != string(rest) || field[int64](t, rest, "id") != is.ID {
			t.Fatalf("read %d of the issue differs:\n%s\n%s", read, issue.text, rest)
		}
	}

	// A repository without description or visibility: public, description null, as POST /user/repos.
	plain := f.ok("5", "create_repo", `{"name":"plain"}`)
	if !strings.Contains(plain.text, `"private":false`) || !strings.Contains(plain.text, `"description":null`) {
		t.Errorf("plain repository: %s", plain.text)
	}
	// A taken name (in any case), an invalid name, wrong types: tool errors in REST's words.
	for arguments, want := range map[string]string{
		`{"name":"` + name + `"}`:                  "name already exists on this account",
		`{"name":"` + strings.ToUpper(name) + `"}`: "name already exists on this account",
		`{"name":"has space"}`:                     "Repository creation failed.",
		`{"name":"x","private":"yes"}`:             "invalid argument: private must be a boolean",
		`{"name":"x","description":5}`:             "invalid argument: description must be a string",
	} {
		if a := f.call("6", "create_repo", arguments); !a.isError || !strings.Contains(a.text, want) {
			t.Errorf("create_repo %s: %s", arguments, a.body)
		}
	}
	if len(f.st.Repos()) != 2 {
		t.Errorf("%d repositories after refused calls", len(f.st.Repos()))
	}
	if a := f.call("7", "get_repo", `{"owner":"octocat","repo":"`+name+`"}`); !a.isError || !strings.Contains(a.text, "Not Found") {
		t.Errorf("another owner's repository: %s", a.body)
	}
}

// Check 15 (mcp.auth-required): without the token nothing is served, whatever the body says.
func TestCheckAuthRequired(t *testing.T) {
	f := newFixture(t)
	f.connect()
	f.repo("no-auth-" + tag)
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_issue","arguments":{"owner":"` + owner + `","repo":"no-auth-` + tag + `","title":"Flaky pack negotiation","body":"x"},` + meta + `}}`
	for _, auth := range []string{"", "Bearer wrong", "Bearer", "Basic " + testToken, testToken} {
		for _, payload := range []string{body, `{"jsonrpc":"2.0","id":"discover-1","method":"server/discover"}`, `not json`, ``} {
			r := parseReply(t, f.tester.Do(request(auth, "tools/call", payload)))
			if r.status != 401 || r.header["www-authenticate"] != "Bearer" || string(r.members["id"]) != "null" || r.code != -32001 {
				t.Errorf("auth %q, body %q: %d %s", auth, payload, r.status, r.body)
			}
		}
	}
	if len(f.st.Repo("no-auth-"+tag).Issues) != 0 {
		t.Fatal("an unauthenticated call created an issue")
	}
	// The schemes REST accepts are accepted here.
	for _, auth := range []string{"Bearer " + testToken, "bearer " + testToken, "token " + testToken} {
		if r := parseReply(t, f.tester.Do(request(auth, "ping", `{"jsonrpc":"2.0","id":1,"method":"ping"}`))); r.status != 200 {
			t.Errorf("auth %q: %d %s", auth, r.status, r.body)
		}
	}
}

// Only POST is served.
func TestOtherMethods(t *testing.T) {
	f := newFixture(t)
	for _, method := range []string{"GET", "DELETE", "PUT", "OPTIONS"} {
		raw := f.tester.Do([]byte(method + " /mcp HTTP/1.1\r\nHost: " + testHost + "\r\nAuthorization: Bearer " + testToken + "\r\n\r\n"))
		r := parseReply(t, raw)
		if r.status != 405 || r.header["allow"] != "POST" || string(r.body) != `{"message":"Method Not Allowed"}` {
			t.Errorf("%s: %q", method, raw)
		}
	}
}

// A bug inside a tool is answered as a JSON-RPC internal error under the request's id, not as the
// engine's 500, and the next request is served normally.
func TestToolPanicIsAnInternalError(t *testing.T) {
	f := newFixture(t)
	saved := tools
	tools = append(append([]tool{}, saved...), tool{name: "explode", run: func(*Server, []byte, *arguments, []byte) ([]byte, payload) {
		panic("boom (expected by TestToolPanicIsAnInternalError)")
	}})
	defer func() { tools = saved }()
	r := f.rpc(`"keep-me"`, "tools/call", `"name":"explode"`)
	if r.status != 200 || r.code != codeInternalError {
		t.Fatalf("panic: %d %s", r.status, r.body)
	}
	if r := f.rpc("2", "ping", ""); r.status != 200 || r.members["result"] == nil {
		t.Fatalf("after a panic: %d %s", r.status, r.body)
	}
}

// A reply much larger than the scratch buffer's first size is still one intact body.
func TestLargeAnswers(t *testing.T) {
	f := newFixture(t)
	r := f.repo("big")
	long := strings.Repeat("long body with \"quotes\" and \n newlines ", 400)
	for i := 0; i < listLimit+20; i++ {
		if _, err := r.CreateIssue(store.NewIssue{Title: "Issue " + strconv.Itoa(i), Body: long}); err != nil {
			t.Fatal(err)
		}
	}
	a := f.ok("1", "list_issues", `{"owner":"`+owner+`","repo":"big"}`)
	var list []struct {
		Number int64
		Body   string
	}
	if err := json.Unmarshal([]byte(a.text), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != listLimit || list[0].Number != int64(listLimit+20) || list[0].Body != long {
		t.Fatalf("%d issues, first #%d", len(list), list[0].Number)
	}
	// The scratch buffer is reused by the next request on the connection.
	if got := f.ok("2", "get_repo", `{"owner":"`+owner+`","repo":"big"}`); field[string](t, got.structured, "name") != "big" {
		t.Errorf("after a large answer: %s", got.body)
	}
}
