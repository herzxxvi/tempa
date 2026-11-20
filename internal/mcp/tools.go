package mcp

import (
	"encoding/json"
	"errors"
	"math"

	"forge/internal/httpd"
	"forge/internal/render"
	"forge/internal/store"
)

// The tools. Each one is a REST operation under another name: it calls the store operation the REST
// handler calls and returns what the REST handler returns, rendered by the same renderer for the
// same host. So ids, numbers and URLs are identical on both surfaces.
//
// This table is the single description of the tools: tools/list is generated from it once, and
// tools/call checks arguments against it, so the advertised schema and the accepted input cannot
// drift apart. It is an ordered slice because its order is the order of tools/list.
var tools = []tool{
	{
		name: "create_repo",
		desc: "Create a new repository owned by the authenticated user. Returns the repository object, as GET /api/v3/repos/{owner}/{repo} returns it.",
		props: []property{
			{name: "name", kind: typeString, desc: "Repository name", required: true},
			{name: "description", kind: typeString, desc: "Repository description"},
			{name: "private", kind: typeBoolean, desc: "Whether the repository is private"},
		},
		run: (*Server).createRepo,
	},
	{
		name: "get_repo",
		desc: "Get a repository by owner and name. Returns the repository object.",
		props: []property{
			{name: "owner", kind: typeString, desc: ownerDesc, required: true},
			{name: "repo", kind: typeString, desc: repoDesc, required: true},
		},
		run: (*Server).getRepo,
	},
	{
		name: "create_issue",
		desc: "Open a new issue in a repository. Returns the issue object, as GET /api/v3/repos/{owner}/{repo}/issues/{number} returns it.",
		props: []property{
			{name: "owner", kind: typeString, desc: ownerDesc, required: true},
			{name: "repo", kind: typeString, desc: repoDesc, required: true},
			{name: "title", kind: typeString, desc: "Issue title", required: true},
			{name: "body", kind: typeString, desc: "Issue body"},
		},
		run: (*Server).createIssue,
	},
	{
		name: "list_issues",
		desc: "List the issues of a repository. Returns a JSON array of issue objects.",
		props: []property{
			{name: "owner", kind: typeString, desc: ownerDesc, required: true},
			{name: "repo", kind: typeString, desc: repoDesc, required: true},
			{name: "state", kind: typeString, enum: []string{"open", "closed", "all"}, desc: "Filter by state (default open)"},
		},
		run: (*Server).listIssues,
	},
	{
		name: "create_pull_request",
		desc: "Open a pull request that merges the head branch into the base branch. Returns the pull request object.",
		props: []property{
			{name: "owner", kind: typeString, desc: ownerDesc, required: true},
			{name: "repo", kind: typeString, desc: repoDesc, required: true},
			{name: "title", kind: typeString, desc: "Pull request title", required: true},
			{name: "body", kind: typeString, desc: "Pull request description"},
			{name: "head", kind: typeString, desc: "Branch that holds the changes", required: true},
			{name: "base", kind: typeString, desc: "Branch to merge into", required: true},
		},
		run: (*Server).createPull,
	},
	{
		name: "merge_pull_request",
		desc: "Merge an open pull request with a squash commit on its base branch. Returns {sha, merged, message}.",
		props: []property{
			{name: "owner", kind: typeString, desc: ownerDesc, required: true},
			{name: "repo", kind: typeString, desc: repoDesc, required: true},
			{name: "pull_number", kind: typeNumber, desc: "Pull request number", required: true},
		},
		run: (*Server).mergePull,
	},
}

const (
	ownerDesc = "Repository owner (user login)"
	repoDesc  = "Repository name"
)

// JSON Schema types of tool arguments.
const (
	typeString  = "string"
	typeBoolean = "boolean"
	typeNumber  = "number"
)

// maxProps is the largest number of properties a tool may declare (the size of arguments.vals).
const maxProps = 6

// listLimit caps list_issues at the largest page the REST API serves, newest first.
const listLimit = 100

// tool is one entry of tools/list and its implementation.
type tool struct {
	name  string
	desc  string
	props []property // in schema order
	// run performs the tool with checked arguments and appends its payload to dst: a JSON object
	// (payloadObject), a JSON array (payloadList) or the text of a failure (payloadError).
	run func(s *Server, host []byte, a *arguments, dst []byte) ([]byte, payload)
}

// property is one member of a tool's inputSchema.
type property struct {
	name     string
	kind     string   // typeString, typeBoolean or typeNumber
	enum     []string // allowed values, for the schema; the tool itself judges the value
	desc     string
	required bool // must be present; a required string must also be non-empty
}

// payload says what a tool appended.
type payload uint8

const (
	payloadError  payload = iota // text of a failed call: the result carries isError true
	payloadObject                // a JSON object: sent as text and as structuredContent
	payloadList                  // a JSON array: sent as text only (structuredContent is an object by definition)
)

// toolsListResult is the result of tools/list, rendered once so that every answer is the same bytes.
var toolsListResult = renderToolsList()

func renderToolsList() []byte {
	b := []byte(`{"tools":[`)
	for i := range tools {
		t := &tools[i]
		if len(t.props) > maxProps {
			panic("mcp: tool " + t.name + " declares too many properties")
		}
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"name":`...)
		b = render.AppendString(b, t.name)
		b = append(b, `,"description":`...)
		b = render.AppendString(b, t.desc)
		b = append(b, `,"inputSchema":{"type":"object","properties":{`...)
		for j, p := range t.props {
			if j > 0 {
				b = append(b, ',')
			}
			b = render.AppendString(b, p.name)
			b = append(b, `:{"type":`...)
			b = render.AppendString(b, p.kind)
			if len(p.enum) > 0 {
				b = append(b, `,"enum":`...)
				b = appendStrings(b, p.enum)
			}
			b = append(b, `,"description":`...)
			b = render.AppendString(b, p.desc)
			b = append(b, '}')
		}
		b = append(b, `},"required":[`...)
		first := true
		for _, p := range t.props {
			if !p.required {
				continue
			}
			if !first {
				b = append(b, ',')
			}
			first = false
			b = render.AppendString(b, p.name)
		}
		b = append(b, `]}}`...)
	}
	return append(b, `]}`...)
}

func appendStrings(dst []byte, list []string) []byte {
	dst = append(dst, '[')
	for i, s := range list {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = render.AppendString(dst, s)
	}
	return append(dst, ']')
}

func findTool(name string) *tool {
	for i := range tools {
		if tools[i].name == name {
			return &tools[i]
		}
	}
	return nil
}

// argument is the decoded value of one property.
type argument struct {
	set    bool // present and not null
	text   string
	number int64
	flag   bool
}

// arguments holds a call's arguments, checked against the tool's schema; vals runs parallel to
// the tool's props.
type arguments struct {
	tool *tool
	vals [maxProps]argument
}

// get returns the argument called name. Asking for a property the tool does not declare is a
// programming error.
func (a *arguments) get(name string) *argument {
	for i := range a.tool.props {
		if a.tool.props[i].name == name {
			return &a.vals[i]
		}
	}
	panic("mcp: tool " + a.tool.name + " has no property " + name)
}

func (a *arguments) text(name string) string { return a.get(name).text }

// decode checks raw, the members of the call's "arguments" object, against the tool's schema and
// fills a. It returns the text of the first problem, or "" when the arguments are usable. Members
// the schema does not declare are ignored; an explicit null counts as absent.
func (t *tool) decode(raw map[string]json.RawMessage, a *arguments) (problem string) {
	a.tool = t
	for i := range t.props {
		p := &t.props[i]
		v := &a.vals[i]
		value, ok := raw[p.name]
		if !ok || string(value) == "null" {
			if p.required {
				return "missing required argument: " + p.name
			}
			continue
		}
		switch p.kind {
		case typeString:
			if json.Unmarshal(value, &v.text) != nil {
				return "invalid argument: " + p.name + " must be a string"
			}
			if v.text == "" && p.required {
				return "missing required argument: " + p.name
			}
		case typeBoolean:
			if json.Unmarshal(value, &v.flag) != nil {
				return "invalid argument: " + p.name + " must be a boolean"
			}
		case typeNumber:
			n, ok := positiveInt(value)
			if !ok {
				return "invalid argument: " + p.name + " must be a positive integer"
			}
			v.number = n
		}
		v.set = true
	}
	return ""
}

// positiveInt reads a positive integer from a JSON number (5, 5.0, 5e0) or from a string of
// digits ("5"), which is how clients without a number type send one.
func positiveInt(value json.RawMessage) (int64, bool) {
	if value[0] == '"' {
		var digits string
		if json.Unmarshal(value, &digits) != nil {
			return 0, false
		}
		n, ok := httpd.ParseInt([]byte(digits))
		return n, ok && n > 0
	}
	var f float64
	if json.Unmarshal(value, &f) != nil || f < 1 || f > 1<<53 || f != math.Trunc(f) {
		return 0, false
	}
	return int64(f), true
}

// callTool is tools/call. A call that cannot be dispatched (no name, unknown tool, arguments that
// are not an object) is a JSON-RPC error; everything a tool itself can object to, a missing
// argument included, is a normal result with isError true, as the protocol asks, so that the model
// on the other side can read the reason and correct itself.
func (s *Server) callTool(c *httpd.Ctx, id []byte, params json.RawMessage) (int, []byte) {
	var p struct {
		Name      json.RawMessage `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var name string
	if json.Unmarshal(params, &p) != nil || json.Unmarshal(p.Name, &name) != nil || name == "" {
		return s.fail(c, 200, id, codeInvalidParams, "Invalid params: missing tool name")
	}
	t := findTool(name)
	if t == nil {
		return s.fail(c, 200, id, codeInvalidParams, "Unknown tool: "+name)
	}
	var raw map[string]json.RawMessage
	if given := trimSpace(p.Arguments); len(given) > 0 && string(given) != "null" {
		if given[0] != '{' || json.Unmarshal(given, &raw) != nil {
			return s.fail(c, 200, id, codeInvalidParams, "Invalid params: arguments must be an object")
		}
	}

	// The payload is rendered at the start of the scratch buffer and the response after it, so the
	// payload can be copied into the response twice: once as the text block, once as it is.
	b := c.Scratch[:0]
	var args arguments
	kind := payloadError
	if problem := t.decode(raw, &args); problem != "" {
		b = append(b, problem...)
	} else {
		b, kind = t.run(s, c.Host, &args, b)
	}
	n := len(b)
	b = appendHead(b, id)
	b = append(b, `,"result":{"content":[{"type":"text","text":`...)
	b = render.AppendString(b, string(b[:n]))
	b = append(b, '}', ']')
	if kind == payloadObject {
		b = append(b, `,"structuredContent":`...)
		b = append(b, b[:n]...)
	}
	if kind == payloadError {
		b = append(b, `,"isError":true}}`...)
	} else {
		b = append(b, `,"isError":false}}`...)
	}
	c.Scratch = b
	return 200, b[n:]
}

// refuse appends the failure of a store operation in the form REST reports it, so the same refusal
// reads the same on both surfaces.
func refuse(dst []byte, err error) ([]byte, payload) {
	var e *store.Error
	if !errors.As(err, &e) {
		panic(err) // the store returns nothing else
	}
	return render.AppendError(dst, e), payloadError
}

// repo resolves the owner and repo arguments as REST resolves {owner}/{repo}: nil for another
// owner or an unknown repository.
func (s *Server) repo(a *arguments) *store.Repo {
	return s.st.Lookup([]byte(a.text("owner")), []byte(a.text("repo")))
}

// createRepo is POST /user/repos.
func (s *Server) createRepo(host []byte, a *arguments, dst []byte) ([]byte, payload) {
	description := a.get("description")
	r, err := s.st.CreateRepo(store.NewRepo{
		Name:           a.text("name"),
		Description:    description.text,
		HasDescription: description.set,
		Private:        a.get("private").flag,
	})
	if err != nil {
		return refuse(dst, err)
	}
	return render.AppendRepo(dst, host, r), payloadObject
}

// getRepo is GET /repos/{owner}/{repo}.
func (s *Server) getRepo(host []byte, a *arguments, dst []byte) ([]byte, payload) {
	r := s.repo(a)
	if r == nil {
		return refuse(dst, store.ErrNotFound)
	}
	return render.AppendRepo(dst, host, r), payloadObject
}

// createIssue is POST /repos/{owner}/{repo}/issues.
func (s *Server) createIssue(host []byte, a *arguments, dst []byte) ([]byte, payload) {
	r := s.repo(a)
	if r == nil {
		return refuse(dst, store.ErrNotFound)
	}
	is, err := r.CreateIssue(store.NewIssue{Title: a.text("title"), Body: a.text("body")})
	if err != nil {
		return refuse(dst, err)
	}
	return render.AppendIssue(dst, host, is), payloadObject
}

// listIssues is GET /repos/{owner}/{repo}/issues?state=...&per_page=100: newest first, pull
// requests excluded.
func (s *Server) listIssues(host []byte, a *arguments, dst []byte) ([]byte, payload) {
	r := s.repo(a)
	if r == nil {
		return refuse(dst, store.ErrNotFound)
	}
	state := store.StateOpen
	if given := a.get("state"); given.set {
		var ok bool
		if state, ok = store.ParseState(given.text); !ok {
			return append(dst, "invalid argument: state must be open, closed or all"...), payloadError
		}
	}
	issues, _ := r.ListIssues(store.IssueQuery{State: state, Limit: listLimit}, nil)
	return render.AppendIssues(dst, host, issues), payloadList
}

// createPull is POST /repos/{owner}/{repo}/pulls.
func (s *Server) createPull(host []byte, a *arguments, dst []byte) ([]byte, payload) {
	r := s.repo(a)
	if r == nil {
		return refuse(dst, store.ErrNotFound)
	}
	p, err := r.CreatePull(store.NewPull{Title: a.text("title"), Body: a.text("body"), Head: a.text("head"), Base: a.text("base")})
	if err != nil {
		return refuse(dst, err)
	}
	return render.AppendPull(dst, host, p), payloadObject
}

// mergePull is PUT /repos/{owner}/{repo}/pulls/{number}/merge with merge_method squash.
func (s *Server) mergePull(host []byte, a *arguments, dst []byte) ([]byte, payload) {
	r := s.repo(a)
	if r == nil {
		return refuse(dst, store.ErrNotFound)
	}
	p := r.Pull(a.get("pull_number").number)
	if p == nil {
		return refuse(dst, store.ErrNotFound)
	}
	commit, err := r.MergePull(p, store.MergeOptions{Method: "squash"})
	if err != nil {
		return refuse(dst, err)
	}
	return render.AppendMergeResult(dst, commit.ID), payloadObject
}
