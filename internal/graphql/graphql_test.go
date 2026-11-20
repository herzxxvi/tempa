package graphql

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"forge/internal/httpd"
	"forge/internal/store"
)

// ---- harness: every request goes through the real HTTP engine ----

const (
	testToken = "tok"
	testHost  = "forge.test:4000"
	testRepo  = "amber-thistle-graphql"
	issueBody = "A minimal case is attached but the second request fails. The same flow works over the API and the server answers with a JSON error. Steps to reproduce are below but the second request fails."
	titleHead = "Incorrect GraphQL node ids"
)

type harness struct {
	t      *testing.T
	st     *store.Store
	tester *httpd.Tester
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := store.New()
	api := New(st)
	return &harness{t: t, st: st, tester: httpd.NewTester(&httpd.Server{Handler: api.Serve, Token: testToken})}
}

// send posts a raw body and returns the status and the reply body.
func (h *harness) send(method, token, body string) (int, []byte) {
	h.t.Helper()
	req := method + " /api/graphql HTTP/1.1\r\nHost: " + testHost + "\r\nContent-Type: application/json\r\n"
	if token != "" {
		req += "Authorization: Bearer " + token + "\r\n"
	}
	req += "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	raw := h.tester.Do([]byte(req))
	head, payload, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		h.t.Fatalf("malformed reply %q", raw)
	}
	status, _ := strconv.Atoi(string(head[9:12]))
	if !bytes.Contains(head, []byte("Content-Type: application/json; charset=utf-8")) {
		h.t.Errorf("reply is not JSON: %s", head)
	}
	return status, bytes.Clone(payload)
}

func requestBody(query string, variables any) string {
	if variables == nil {
		variables = map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// reply is a decoded GraphQL reply object.
type reply struct {
	raw    string
	data   map[string]any
	errors []any
	keys   map[string]any
}

func decodeReply(t *testing.T, raw []byte) reply {
	t.Helper()
	r := reply{raw: string(raw)}
	if err := json.Unmarshal(raw, &r.keys); err != nil {
		t.Fatalf("reply is not a JSON object: %s", raw)
	}
	r.data, _ = r.keys["data"].(map[string]any)
	r.errors, _ = r.keys["errors"].([]any)
	return r
}

// post runs one operation and returns its reply; the status must be 200.
func (h *harness) post(query string, variables any) reply {
	h.t.Helper()
	status, body := h.send("POST", testToken, requestBody(query, variables))
	if status != 200 {
		h.t.Fatalf("status %d: %s", status, body)
	}
	return decodeReply(h.t, body)
}

// data runs one operation that must succeed without errors, as the suite's dataOf demands.
func (h *harness) data(query string, variables any) map[string]any {
	h.t.Helper()
	r := h.post(query, variables)
	if _, has := r.keys["errors"]; has || r.data == nil {
		h.t.Fatalf("expected data and no errors, got %s\nquery: %s", r.raw, query)
	}
	return r.data
}

// failing runs one operation that must be refused before execution, and returns its messages.
func (h *harness) failing(query string, variables any) []string {
	h.t.Helper()
	r := h.post(query, variables)
	if _, has := r.keys["data"]; has || len(r.errors) == 0 {
		h.t.Fatalf("expected errors and no data, got %s\nquery: %s", r.raw, query)
	}
	return messages(r.errors)
}

func messages(errs []any) []string {
	var out []string
	for _, e := range errs {
		out = append(out, fmt.Sprint(at(e, "message")))
	}
	return out
}

// at reads a nested value: string keys index objects, int keys index arrays.
func at(v any, path ...any) any {
	for _, p := range path {
		switch key := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[key]
		case int:
			l, ok := v.([]any)
			if !ok || key >= len(l) {
				return nil
			}
			v = l[key]
		}
	}
	return v
}

func names(list any) []string {
	var out []string
	for _, e := range list.([]any) {
		out = append(out, at(e, "name").(string))
	}
	return out
}

func numbers(nodes any) []int {
	var out []int
	for _, n := range nodes.([]any) {
		out = append(out, int(at(n, "number").(float64)))
	}
	return out
}

func contains(list []string, want ...string) bool {
next:
	for _, w := range want {
		for _, s := range list {
			if s == w {
				continue next
			}
		}
		return false
	}
	return true
}

func must[T any](t *testing.T) func(v T, err error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// ---- the suite's documents, verbatim from spec/GRAPHQL.md ----

const viewerLogin = `query ViewerLogin {
  viewer {
    login
  }
}`

const repositoryFields = `query RepositoryFields($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    id
    databaseId
    name
    owner {
      login
    }
    defaultBranchRef {
      name
    }
    viewerPermission
    hasIssuesEnabled
  }
}`

const issueFields = `query Issue_fields {
  Issue: __type(name: "Issue") {
    fields(includeDeprecated: true) {
      name
    }
  }
}`

const typeFields = `query PullRequestAndRepositoryFields {
  PullRequest: __type(name: "PullRequest") {
    fields(includeDeprecated: true) {
      name
    }
  }
  Repository: __type(name: "Repository") {
    fields(includeDeprecated: true) {
      name
    }
  }
}`

const searchTypeEnumValues = `query SearchType_enumValues {
  SearchType: __type(name: "SearchType") {
    enumValues(includeDeprecated: true) {
      name
    }
  }
}`

const openIssues = `query OpenIssues($owner: String!, $name: String!, $first: Int!) {
  repository(owner: $owner, name: $name) {
    issues(first: $first, states: [OPEN], orderBy: {field: CREATED_AT, direction: DESC}) {
      nodes {
        number
        title
        state
        id
      }
    }
  }
}`

const issueByNumber = `query IssueByNumber($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      number
      title
      state
      id
      body
    }
  }
}`

const createIssue = `mutation IssueCreate($input: CreateIssueInput!) {
  createIssue(input: $input) {
    issue {
      number
      id
    }
  }
}`

const closeIssue = `mutation IssueClose($input: CloseIssueInput!) {
  closeIssue(input: $input) {
    issue {
      number
      id
      state
    }
  }
}`

const pullRequests = `query PullRequests($owner: String!, $name: String!, $first: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequests(first: $first) {
      nodes {
        number
        headRefName
        baseRefName
        mergeable
        state
      }
    }
  }
}`

const pullRequestFiles = `query PullRequestFiles($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      number
      files(first: 10) {
        nodes {
          path
        }
      }
    }
  }
}`

const pullRequestID = `query PullRequestId($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      id
      number
    }
  }
}`

const createPullRequest = `mutation PullRequestCreate($input: CreatePullRequestInput!) {
  createPullRequest(input: $input) {
    pullRequest {
      number
      id
      state
      headRefName
      baseRefName
    }
  }
}`

const mergePullRequest = `mutation MergePullRequest($input: MergePullRequestInput!) {
  mergePullRequest(input: $input) {
    pullRequest {
      number
      merged
      state
    }
  }
}`

const addComment = `mutation AddComment($input: AddCommentInput!) {
  addComment(input: $input) {
    commentEdge {
      node {
        id
        body
      }
    }
  }
}`

const invalidField = `query ViewerInvalidField {
  viewer {
    login
    noSuchFieldOnViewer
  }
}`

const nodeRepository = `query NodeRepository($id: ID!) {
  node(id: $id) {
    id
    ... on Repository {
      name
      nameWithOwner
    }
  }
}`

const issuesPage = `query IssuesPage($owner: String!, $name: String!, $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    issues(first: $first, after: $after) {
      nodes {
        number
      }
      pageInfo {
        hasNextPage
        endCursor
      }
    }
  }
}`

var repoVars = map[string]any{"owner": store.ViewerLogin, "name": testRepo}

func withVars(extra map[string]any) map[string]any {
	out := map[string]any{"owner": store.ViewerLogin, "name": testRepo}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// push stands in for the suite's git fixture: one commit that adds a file on a branch.
func push(t *testing.T, r *store.Repo, branch, path, message string) {
	t.Helper()
	must[store.FileResult](t)(r.PutFile(store.FileWrite{Path: path, Message: message, Data: []byte(issueBody + "\n"), Branch: branch}))
}

// TestSuiteDocuments walks the twenty checks of the conformance suite in their order, with the
// fixtures the suite builds over REST and git built through the store instead, and asserts what
// each check asserts.
func TestSuiteDocuments(t *testing.T) {
	h := newHarness(t)

	// 1 viewer-login
	if got := at(h.data(viewerLogin, nil), "viewer", "login"); got != "adaiversen" {
		t.Fatalf("viewer.login = %v", got)
	}

	// 2 repository-fields: first on the empty repository (no error, defaultBranchRef null) ...
	r := must[*store.Repo](t)(h.st.CreateRepo(store.NewRepo{Name: testRepo, Description: "the tooling behind the thing", HasDescription: true}))
	empty := h.data(repositoryFields, repoVars)["repository"].(map[string]any)
	if ref, has := empty["defaultBranchRef"]; !has || ref != nil {
		t.Fatalf("empty repository: defaultBranchRef = %v", ref)
	}
	repoID, _ := empty["id"].(string)
	if repoID == "" || repoID != r.NodeID || empty["databaseId"] != float64(r.ID) {
		t.Fatalf("empty repository: id %v databaseId %v", empty["id"], empty["databaseId"])
	}
	// ... then after the first push to main.
	push(t, r, "", "server.toml", "fixture: first commit on main\n")
	want := map[string]any{
		"id": repoID, "databaseId": float64(r.ID), "name": testRepo,
		"owner":            map[string]any{"login": "adaiversen"},
		"defaultBranchRef": map[string]any{"name": "main"},
		"viewerPermission": "ADMIN", "hasIssuesEnabled": true,
	}
	if got := h.data(repositoryFields, repoVars)["repository"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("repository = %v", got)
	}

	// 3 introspection-issue
	if got := names(at(h.data(issueFields, nil), "Issue", "fields")); !contains(got, "id", "number", "title", "state", "body") {
		t.Fatalf("Issue fields = %v", got)
	}

	// 4 introspection-types
	types := h.data(typeFields, nil)
	if got := names(at(types, "PullRequest", "fields")); !contains(got, "id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files") {
		t.Fatalf("PullRequest fields = %v", got)
	}
	if got := names(at(types, "Repository", "fields")); !contains(got, "id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest") {
		t.Fatalf("Repository fields = %v", got)
	}
	if got := names(at(h.data(searchTypeEnumValues, nil), "SearchType", "enumValues")); !contains(got, "ISSUE") {
		t.Fatalf("SearchType values = %v", got)
	}

	// 5 issues-open-order
	for _, suffix := range []string{"open 1", "open 2", "open 3", "closed"} {
		must[*store.Issue](t)(r.CreateIssue(store.NewIssue{Title: titleHead + " " + suffix, Body: issueBody}))
	}
	closed := "closed"
	if err := r.UpdateIssue(r.Issue(4), store.IssuePatch{State: &closed}); err != nil {
		t.Fatal(err)
	}
	nodes := at(h.data(openIssues, withVars(map[string]any{"first": 10})), "repository", "issues", "nodes")
	if got := numbers(nodes); !reflect.DeepEqual(got, []int{3, 2, 1}) {
		t.Fatalf("open issues = %v", got)
	}
	for _, n := range nodes.([]any) {
		if at(n, "state") != "OPEN" || at(n, "id") == "" || !strings.HasPrefix(at(n, "title").(string), titleHead) {
			t.Fatalf("open issue node = %v", n)
		}
	}

	// 6 issue-by-number
	issue := at(h.data(issueByNumber, withVars(map[string]any{"number": 1})), "repository", "issue").(map[string]any)
	if issue["number"] != 1.0 || issue["state"] != "OPEN" || issue["title"] != titleHead+" open 1" || issue["body"] != issueBody || issue["id"] == "" {
		t.Fatalf("issue = %v", issue)
	}

	// 7 create-issue
	created := at(h.data(createIssue, map[string]any{"input": map[string]any{"repositoryId": repoID, "title": titleHead + " created over graphql", "body": issueBody}}), "createIssue", "issue")
	if at(created, "number") != 5.0 || at(created, "id") == "" {
		t.Fatalf("createIssue = %v", created)
	}
	if is := r.Issue(5); is == nil || is.Title != titleHead+" created over graphql" || is.Body != issueBody || is.Closed {
		t.Fatalf("the store does not have the created issue: %+v", is)
	}

	// 8 close-issue
	must[*store.Issue](t)(r.CreateIssue(store.NewIssue{Title: titleHead + " closed over graphql", Body: issueBody}))
	id := at(h.data(issueByNumber, withVars(map[string]any{"number": 6})), "repository", "issue", "id")
	closedIssue := at(h.data(closeIssue, map[string]any{"input": map[string]any{"issueId": id}}), "closeIssue", "issue")
	if at(closedIssue, "number") != 6.0 || at(closedIssue, "state") != "CLOSED" || at(closedIssue, "id") != id {
		t.Fatalf("closeIssue = %v", closedIssue)
	}
	if is := r.Issue(6); !is.Closed || is.StateReason != "completed" || is.ClosedAt == 0 {
		t.Fatalf("the store's issue is not closed: %+v", is)
	}

	// 9 pull-requests-list
	push(t, r, "chore/mcp-tool-schemas", "config/token.css", "fixture: add config/token.css\n")
	push(t, r, "chore/conditional-requests", "config/token.css.create", "fixture: branch for createPullRequest\n")
	push(t, r, "feature/repo-pagination", "config/token.css.merge", "fixture: branch for mergePullRequest\n")
	fixture := must[*store.Pull](t)(r.CreatePull(store.NewPull{Title: titleHead + " fixture pull request", Body: issueBody, Head: "chore/mcp-tool-schemas", Base: "main"}))
	listed := at(h.data(pullRequests, withVars(map[string]any{"first": 5})), "repository", "pullRequests", "nodes").([]any)
	wantPull := map[string]any{"number": float64(fixture.Number), "headRefName": "chore/mcp-tool-schemas", "baseRefName": "main", "mergeable": "MERGEABLE", "state": "OPEN"}
	if len(listed) != 1 || !reflect.DeepEqual(listed[0], wantPull) {
		t.Fatalf("pullRequests = %v", listed)
	}

	// 10 pull-request-files
	files := at(h.data(pullRequestFiles, withVars(map[string]any{"number": fixture.Number})), "repository", "pullRequest")
	if at(files, "number") != float64(fixture.Number) || !reflect.DeepEqual(at(files, "files", "nodes"), []any{map[string]any{"path": "config/token.css"}}) {
		t.Fatalf("pullRequest files = %v", files)
	}

	// 11 create-pull-request (no body, no draft in the input)
	pr := at(h.data(createPullRequest, map[string]any{"input": map[string]any{"repositoryId": repoID, "baseRefName": "main", "headRefName": "chore/conditional-requests", "title": titleHead + " created over graphql"}}), "createPullRequest", "pullRequest")
	if at(pr, "number") != 8.0 || at(pr, "state") != "OPEN" || at(pr, "headRefName") != "chore/conditional-requests" || at(pr, "baseRefName") != "main" || at(pr, "id") == "" {
		t.Fatalf("createPullRequest = %v", pr)
	}
	if p := r.Pull(8); p == nil || p.HeadRef != "chore/conditional-requests" || p.BaseRef != "main" || p.Body != "" || p.Draft {
		t.Fatalf("the store does not have the created pull request: %+v", p)
	}

	// 12 merge-pull-request
	merge := must[*store.Pull](t)(r.CreatePull(store.NewPull{Title: titleHead + " merge pull request", Body: issueBody, Head: "feature/repo-pagination", Base: "main"}))
	mainBefore, _ := r.Branch("main")
	pullID := at(h.data(pullRequestID, withVars(map[string]any{"number": merge.Number})), "repository", "pullRequest", "id")
	merged := at(h.data(mergePullRequest, map[string]any{"input": map[string]any{"pullRequestId": pullID, "mergeMethod": "SQUASH"}}), "mergePullRequest", "pullRequest")
	if at(merged, "merged") != true || at(merged, "state") != "MERGED" || at(merged, "number") != float64(merge.Number) {
		t.Fatalf("mergePullRequest = %v", merged)
	}
	mainAfter, _ := r.Branch("main")
	squash := r.Objects.GetCommit(mainAfter)
	if !merge.Merged || !merge.Closed || mainAfter == mainBefore || len(squash.Commit.Parents) != 1 || squash.Commit.Parents[0] != mainBefore {
		t.Fatalf("the store's pull request was not squash-merged: %+v", merge)
	}

	// 13 add-comment
	const comment = "The same flow works over the API and nothing is written to the log. The same flow works over the API and the status is wrong for the method. The same flow works over the API and nothing is written to the log."
	commented := must[*store.Issue](t)(r.CreateIssue(store.NewIssue{Title: titleHead + " commented over graphql", Body: issueBody}))
	id = at(h.data(issueByNumber, withVars(map[string]any{"number": commented.Number})), "repository", "issue", "id")
	node := at(h.data(addComment, map[string]any{"input": map[string]any{"subjectId": id, "body": comment}}), "addComment", "commentEdge", "node")
	if at(node, "body") != comment || at(node, "id") == "" {
		t.Fatalf("addComment = %v", node)
	}
	if list, _ := r.Comments(commented.Number); len(list) != 1 || list[0].Body != comment || store.NodeID(store.KindComment, list[0].ID) != at(node, "id") {
		t.Fatalf("the store does not have the comment: %v", list)
	}

	// 14 invalid-field: a 200 with GitHub's error and no data
	status, body := h.send("POST", testToken, requestBody(invalidField, nil))
	const wantInvalid = `{"errors":[{"path":["query ViewerInvalidField","viewer","noSuchFieldOnViewer"],"extensions":{"code":"undefinedField","typeName":"User","fieldName":"noSuchFieldOnViewer"},"locations":[{"line":4,"column":5}],"message":"Field 'noSuchFieldOnViewer' doesn't exist on type 'User'"}]}`
	if status != 200 || string(body) != wantInvalid {
		t.Fatalf("invalid field: %d %s", status, body)
	}

	// 15 unauthenticated
	if status, _ := h.send("POST", "", requestBody(viewerLogin, nil)); status != 401 {
		t.Fatalf("unauthenticated: status %d", status)
	}

	// 16 node-repository
	wantNode := map[string]any{"id": repoID, "name": testRepo, "nameWithOwner": "adaiversen/" + testRepo}
	if got := h.data(nodeRepository, map[string]any{"id": repoID})["node"]; !reflect.DeepEqual(got, wantNode) {
		t.Fatalf("node = %v", got)
	}

	// 17 unknown-repository: null, the key present, no errors
	status, body = h.send("POST", testToken, requestBody(repositoryFields, map[string]any{"owner": "adaiversen", "name": testRepo + "-missing"}))
	if status != 200 || string(body) != `{"data":{"repository":null}}` {
		t.Fatalf("unknown repository: %d %s", status, body)
	}

	// 18 batched-operations
	batch := "[" + requestBody(viewerLogin, nil) + "," + requestBody(issueFields, nil) + "]"
	status, body = h.send("POST", testToken, batch)
	var replies []json.RawMessage
	if err := json.Unmarshal(body, &replies); status != 200 || err != nil || len(replies) != 2 {
		t.Fatalf("batch: %d %s", status, body)
	}
	if string(replies[0]) != `{"data":{"viewer":{"login":"adaiversen"}}}` {
		t.Fatalf("batch entry 0 = %s", replies[0])
	}
	if second := decodeReply(t, replies[1]); second.errors != nil || len(names(at(second.data, "Issue", "fields"))) == 0 {
		t.Fatalf("batch entry 1 = %s", replies[1])
	}

	// 19 global-id-stability
	again := at(h.data(repositoryFields, repoVars), "repository", "id")
	if again != repoID || repoID == strconv.FormatInt(r.ID, 10) {
		t.Fatalf("repository id changed or is the database id: %v then %v", repoID, again)
	}
	first := at(h.data(issueByNumber, withVars(map[string]any{"number": 1})), "repository", "issue", "id")
	second := at(h.data(issueByNumber, withVars(map[string]any{"number": 1})), "repository", "issue", "id")
	if first != second || first == "1" || first != store.NodeID(store.KindIssue, r.Issue(1).ID) {
		t.Fatalf("issue id: %v then %v", first, second)
	}

	// 20 issues-pagination (after: null is sent explicitly on the first page)
	pages := must[*store.Repo](t)(h.st.CreateRepo(store.NewRepo{Name: testRepo + "-pages"}))
	for i := 1; i <= 7; i++ {
		must[*store.Issue](t)(pages.CreateIssue(store.NewIssue{Title: titleHead + " page " + strconv.Itoa(i), Body: issueBody}))
	}
	pageVars := map[string]any{"owner": "adaiversen", "name": pages.Name, "first": 5, "after": nil}
	page := at(h.data(issuesPage, pageVars), "repository", "issues")
	cursor, _ := at(page, "pageInfo", "endCursor").(string)
	if got := numbers(at(page, "nodes")); !reflect.DeepEqual(got, []int{1, 2, 3, 4, 5}) || at(page, "pageInfo", "hasNextPage") != true || cursor == "" {
		t.Fatalf("page 1 = %v", page)
	}
	pageVars["after"] = cursor
	page = at(h.data(issuesPage, pageVars), "repository", "issues")
	if got := numbers(at(page, "nodes")); !reflect.DeepEqual(got, []int{6, 7}) || at(page, "pageInfo", "hasNextPage") != false {
		t.Fatalf("page 2 = %v", page)
	}
}

// ---- transport ----

func TestTransport(t *testing.T) {
	h := newHarness(t)
	body := requestBody(viewerLogin, nil)

	if status, reply := h.send("POST", "", body); status != 401 || !strings.Contains(string(reply), "requires you to be authenticated") {
		t.Errorf("no credentials: %d %s", status, reply)
	}
	// Authentication comes before the body, whatever the body is.
	for _, bad := range []string{"", "not json", "[" + body + "]"} {
		if status, _ := h.send("POST", "", bad); status != 401 {
			t.Errorf("no credentials, body %q: %d", bad, status)
		}
	}
	if status, reply := h.send("POST", "wrong", body); status != 401 || !strings.Contains(string(reply), "Bad credentials") {
		t.Errorf("wrong token: %d %s", status, reply)
	}
	if status, _ := h.send("GET", testToken, ""); status != 404 {
		t.Errorf("GET: %d", status)
	}
	for _, bad := range []string{"", "   ", "not json", `{"query":`, `[{"query":"{viewer{login}}"}`, `"a string"`, `42`} {
		if status, reply := h.send("POST", testToken, bad); status != 400 || !strings.Contains(string(reply), "Problems parsing JSON") {
			t.Errorf("body %q: %d %s", bad, status, reply)
		}
	}
	for _, missing := range []string{`{}`, `{"query":null}`, `{"query":42}`, `{"variables":{}}`} {
		status, reply := h.send("POST", testToken, missing)
		if status != 200 || string(reply) != `{"errors":[{"message":"A query attribute must be specified and must be a string."}]}` {
			t.Errorf("body %s: %d %s", missing, status, reply)
		}
	}
	for _, ok := range []string{
		`{"query":"{viewer{login}}"}`,
		`{"query":"{viewer{login}}","variables":null}`,
		`{"query":"{viewer{login}}","variables":"","operationName":null}`,
		`{"query":"query($n: Int){viewer{login avatarUrl(size:$n)}}","variables":"{\"n\":1}"}`,
		"  \r\n" + `{"query":"{viewer{login}}","extensions":{"anything":1}}`,
	} {
		status, reply := h.send("POST", testToken, ok)
		if status != 200 || !strings.HasPrefix(string(reply), `{"data":{"viewer":{"login":"adaiversen"`) {
			t.Errorf("body %s: %d %s", ok, status, reply)
		}
	}
	if status, reply := h.send("POST", testToken, `{"query":"{viewer{login}}","variables":[1]}`); status != 200 || !strings.Contains(string(reply), "variables must be a JSON object") {
		t.Errorf("array variables: %d %s", status, reply)
	}
}

func TestBatch(t *testing.T) {
	h := newHarness(t)
	r := must[*store.Repo](t)(h.st.CreateRepo(store.NewRepo{Name: "batch"}))
	if status, reply := h.send("POST", testToken, "[]"); status != 200 || string(reply) != "[]" {
		t.Errorf("empty batch: %d %s", status, reply)
	}
	// Entries run in order and independently: the second sees the first's issue, the third fails
	// alone, the fourth is not an operation at all.
	create := requestBody(`mutation($id: ID!) { createIssue(input: {repositoryId: $id, title: "from a batch"}) { issue { number } } }`, map[string]any{"id": r.NodeID})
	read := requestBody(`{ repository(owner: "adaiversen", name: "batch") { issue(number: 1) { title } } }`, nil)
	status, reply := h.send("POST", testToken, "["+create+","+read+","+requestBody("{ nope }", nil)+`,7]`)
	want := `[{"data":{"createIssue":{"issue":{"number":1}}}},` +
		`{"data":{"repository":{"issue":{"title":"from a batch"}}}},` +
		`{"errors":[{"path":["query","nope"],"extensions":{"code":"undefinedField","typeName":"Query","fieldName":"nope"},"locations":[{"line":1,"column":3}],"message":"Field 'nope' doesn't exist on type 'Query'"}]},` +
		`{"errors":[{"message":"A batched operation must be a JSON object."}]}]`
	if status != 200 || string(reply) != want {
		t.Errorf("batch: %d\n got %s\nwant %s", status, reply, want)
	}
	// A single operation never gets an array back.
	if _, reply := h.send("POST", testToken, requestBody(viewerLogin, nil)); reply[0] != '{' {
		t.Errorf("single operation answered with %s", reply)
	}
}

// ---- language features ----

func TestLanguage(t *testing.T) {
	h := newHarness(t)
	r := must[*store.Repo](t)(h.st.CreateRepo(store.NewRepo{Name: "lang"}))
	must[*store.Issue](t)(r.CreateIssue(store.NewIssue{Title: "first", Body: "one"}))
	must[*store.Issue](t)(r.CreateIssue(store.NewIssue{Title: "second"}))
	closed := "closed"
	if err := r.UpdateIssue(r.Issue(2), store.IssuePatch{State: &closed}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		query string
		vars  map[string]any
		op    string
		want  string
	}{
		{name: "shorthand", query: `{ viewer { login } }`, want: `{"viewer":{"login":"adaiversen"}}`},
		{name: "aliases, __typename, key order", query: `{ me: viewer { __typename name: login login } __typename }`,
			want: `{"me":{"__typename":"User","name":"adaiversen","login":"adaiversen"},"__typename":"Query"}`},
		{name: "comments, commas, a byte order mark",
			query: "\xEF\xBB\xBF# leading comment\r\nquery Q , { viewer , { login , } } # trailing",
			want:  `{"viewer":{"login":"adaiversen"}}`},
		{name: "named fragment and spread",
			query: `query { repository(owner: "adaiversen", name: "lang") { ...Parts } } fragment Parts on Repository { name issue(number: 1) { ...Text } } fragment Text on Issue { title body }`,
			want:  `{"repository":{"name":"lang","issue":{"title":"first","body":"one"}}}`},
		{name: "inline fragments on an interface",
			query: `query($id: ID!) { node(id: $id) { __typename ... on Issue { number } ... on Repository { name } ... { id } ... on Node { again: id } } }`,
			vars:  map[string]any{"id": r.NodeID},
			want:  `{"node":{"__typename":"Repository","name":"lang","id":"` + r.NodeID + `","again":"` + r.NodeID + `"}}`},
		{name: "fields with one key are merged",
			query: `{ repository(owner: "adaiversen", name: "lang") { name } repository(owner: "adaiversen", name: "lang") { isEmpty name } }`,
			want:  `{"repository":{"name":"lang","isEmpty":true}}`},
		{name: "variable defaults", query: `query($owner: String = "adaiversen", $name: String! = "lang", $n: Int = 2) { repository(owner: $owner, name: $name) { issue(number: $n) { title } } }`,
			want: `{"repository":{"issue":{"title":"second"}}}`},
		{name: "a variable overrides its default", query: `query($n: Int = 2) { repository(owner: "adaiversen", name: "lang") { issue(number: $n) { title } } }`,
			vars: map[string]any{"n": 1}, want: `{"repository":{"issue":{"title":"first"}}}`},
		{name: "skip and include", query: `query($yes: Boolean!, $no: Boolean = false) { viewer { login @skip(if: $yes) name @include(if: $yes) email @include(if: $no) id @skip(if: $no) @unknownDirective } }`,
			vars: map[string]any{"yes": true}, want: `{"viewer":{"name":"ada iversen","id":"` + store.ViewerNodeID + `"}}`},
		{name: "skipped fragments", query: `{ viewer { ...A @skip(if: true) ... @include(if: false) { name } ... on User @include(if: true) { login } } } fragment A on User { email }`,
			want: `{"viewer":{"login":"adaiversen"}}`},
		{name: "operation chosen by name", query: `query A { viewer { login } } query B { viewer { name } }`, op: "B",
			want: `{"viewer":{"name":"ada iversen"}}`},
		{name: "block string argument", query: "{ repository(owner: \"\"\"\n    adaiversen\n  \"\"\", name: \"lang\") { name } }",
			want: `{"repository":{"name":"lang"}}`},
		{name: "list and enum literals, a single value for a list", query: `{ repository(owner: "adaiversen", name: "lang") { a: issues(states: [CLOSED], first: 5) { nodes { number } } b: issues(states: OPEN, first: 5) { totalCount } } }`,
			want: `{"repository":{"a":{"nodes":[{"number":2}]},"b":{"totalCount":1}}}`},
		{name: "enum and list variables", query: `query($states: [IssueState!], $order: IssueOrder) { repository(owner: "adaiversen", name: "lang") { issues(states: $states, orderBy: $order, first: 5) { nodes { number } } } }`,
			vars: map[string]any{"states": []string{"OPEN", "CLOSED"}, "order": map[string]any{"field": "CREATED_AT", "direction": "DESC"}},
			want: `{"repository":{"issues":{"nodes":[{"number":2},{"number":1}]}}}`},
		{name: "a variable inside an input object literal", query: `query($dir: OrderDirection!) { repository(owner: "adaiversen", name: "lang") { issues(orderBy: {field: CREATED_AT, direction: $dir}, first: 1) { nodes { number } } } }`,
			vars: map[string]any{"dir": "DESC"}, want: `{"repository":{"issues":{"nodes":[{"number":2}]}}}`},
		{name: "null literal and null variable", query: `query($after: String) { repository(owner: "adaiversen", name: "lang") { a: issues(first: 1, after: null) { nodes { number } } b: issues(first: 1, after: $after) { nodes { number } } } }`,
			vars: map[string]any{"after": nil}, want: `{"repository":{"a":{"nodes":[{"number":1}]},"b":{"nodes":[{"number":1}]}}}`},
		{name: "strings round-trip", query: `mutation($id: ID!, $body: String!) { addComment(input: {subjectId: $id, body: $body}) { commentEdge { node { body } } subject { ... on Issue { number } } clientMutationId } }`,
			vars: map[string]any{"id": store.NodeID(store.KindIssue, 1), "body": "<b>&amp;</b> \"quoted\" \\ back\nline\ttab \u00e9\u4e16\U0001F600"},
			want: `{"addComment":{"commentEdge":{"node":{"body":"<b>&amp;</b> \"quoted\" \\ back\nline\ttab ` + "\u00e9\u4e16\U0001F600" + `"}},"subject":{"number":1},"clientMutationId":null}}`},
	}
	for _, c := range cases {
		body := map[string]any{"query": c.query, "variables": c.vars}
		if c.op != "" {
			body["operationName"] = c.op
		}
		raw, _ := json.Marshal(body)
		status, reply := h.send("POST", testToken, string(raw))
		if want := `{"data":` + c.want + `}`; status != 200 || string(reply) != want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, reply, want)
		}
	}
}

// ---- errors ----

func TestValidationErrors(t *testing.T) {
	h := newHarness(t)
	r := must[*store.Repo](t)(h.st.CreateRepo(store.NewRepo{Name: "errors"}))
	cases := []struct {
		name  string
		query string
		vars  map[string]any
		want  string // a message the reply must carry
	}{
		{"syntax", "query {\n  viewer {\n    login\n  }\n  }\n}", nil, `Parse error on "}" (RCURLY) at [6, 1]`},
		{"empty document", "  # nothing\n", nil, "Unexpected end of document"},
		{"empty selection", "{ viewer { } }", nil, `Parse error on "}" (RCURLY) at [1, 12]`},
		{"unterminated string", `{ repository(owner: "x`, nil, "Parse error on an unterminated string (error) at [1, 21]"},
		{"bad character", "{ viewer { login } } %", nil, `Parse error on "%" (error) at [1, 22]`},
		{"unknown field on a nested type", `{ repository(owner: "a", name: "b") { issues(first: 1) { nodes { nope } } } }`, nil, "Field 'nope' doesn't exist on type 'Issue'"},
		{"unknown argument", `{ repository(owner: "a", name: "b") { issues(foo: 1) { totalCount } } }`, nil, "Field 'issues' doesn't accept argument 'foo'"},
		{"missing argument", `{ repository(owner: "a") { name } }`, nil, "Field 'repository' is missing required arguments: name"},
		{"duplicate argument", `{ repository(owner: "a", name: "b", name: "c") { name } }`, nil, `There can be only one argument named "name"`},
		{"missing selection", `{ viewer }`, nil, "Field must have selections (field 'viewer' returns User but has no selections. Did you mean 'viewer { ... }'?)"},
		{"selection on a scalar", `{ viewer { login { x } } }`, nil, "Selections can't be made on scalars (field 'login' returns String but has selections [x])"},
		{"literal of the wrong type", `{ repository(owner: 1, name: "b") { name } }`, nil, "Argument 'owner' on Field 'repository' has an invalid value (1). Expected type 'String!'."},
		{"enum given as a string", `{ repository(owner: "a", name: "b") { issues(states: ["OPEN"]) { totalCount } } }`, nil, `Argument 'states' on Field 'issues' has an invalid value (["OPEN"]). Expected type '[IssueState!]'.`},
		{"unknown enum value", `{ search(query: "x", type: CODE) { issueCount } }`, nil, "Argument 'type' on Field 'search' has an invalid value (CODE). Expected type 'SearchType!'."},
		{"null for a non-null argument", `{ node(id: null) { id } }`, nil, "Argument 'id' on Field 'node' has an invalid value (null). Expected type 'ID!'."},
		{"undeclared variable", `query Q { node(id: $id) { id } }`, nil, "Variable $id is used by Q but not declared"},
		{"unused variable", `query Q($id: ID) { viewer { login } }`, nil, "Variable $id is declared by Q but not used"},
		{"variable of the wrong type", `query($id: Int!) { node(id: $id) { id } }`, map[string]any{"id": 1}, "Type mismatch on variable $id and argument id (Int! / ID!)"},
		{"nullable variable for a non-null argument", `query($id: ID) { node(id: $id) { id } }`, nil, "Nullability mismatch on variable $id and argument id (ID / ID!)"},
		{"variable used in a fragment", `query Q { viewer { ...F } } fragment F on User { avatarUrl(size: $size) }`, nil, "Variable $size is used by Q but not declared"},
		{"unknown variable type", `query($x: Nope) { node(id: $x) { id } }`, nil, "Nope isn't a defined input type (on $x)"},
		{"output type as a variable type", `query($x: Issue) { node(id: $x) { id } }`, nil, "Issue isn't a valid input type (on $x)"},
		{"default of the wrong type", `query($n: Int = "x") { repository(owner: "a", name: "b") { issue(number: $n) { id } } }`, nil, "Default value for $n doesn't match type Int"},
		{"unknown fragment", `{ viewer { ...Nope } }`, nil, "Fragment Nope was used, but not defined"},
		{"unused fragment", `{ viewer { login } } fragment F on User { login }`, nil, "Fragment F was defined, but not used"},
		{"fragment on an unknown type", `{ viewer { ... on Nope { login } } }`, nil, "No such type Nope, so it can't be a fragment condition"},
		{"fragment on a scalar", `{ viewer { ... on String { login } } }`, nil, "Invalid fragment on type String (must be Union, Interface, or Object)"},
		{"impossible fragment", `{ viewer { ... on Issue { number } } }`, nil, "Fragment on Issue can't be spread inside User"},
		{"fragment cycle", `{ viewer { ...A } } fragment A on User { ...B } fragment B on User { ...A }`, nil, "Fragment A contains an infinite loop"},
		{"conflicting fields", `{ viewer { x: login x: name } }`, nil, "Field 'x' has a field conflict: login or name?"},
		{"conflicting arguments", `{ viewer { avatarUrl(size: 1) ...F } } fragment F on User { avatarUrl(size: 2) }`, nil, "Field 'avatarUrl' has an argument conflict: {size:1} or {size:2}?"},
		{"two anonymous operations", `{ viewer { login } } { viewer { name } }`, nil, "Operation name is required when multiple operations are present"},
		{"duplicate operation name", `query A { viewer { login } } query A { viewer { name } }`, nil, `Operation name "A" must be unique`},
		{"subscription", `subscription { viewer { login } }`, nil, "Schema is not configured for subscriptions"},
		{"meta field off the root", `{ viewer { __type(name: "User") { name } } }`, nil, "Field '__type' doesn't exist on type 'User'"},
		{"missing variable", `query($owner: String!) { repository(owner: $owner, name: "b") { name } }`, nil, "Variable $owner of type String! was provided invalid value"},
		{"null variable", `query($owner: String!) { repository(owner: $owner, name: "b") { name } }`, map[string]any{"owner": nil}, "Variable $owner of type String! was provided invalid value"},
		{"variable of the wrong JSON type", `query($n: Int!) { repository(owner: "a", name: "b") { issue(number: $n) { id } } }`, map[string]any{"n": "1"}, "Variable $n of type Int! was provided invalid value"},
		{"fractional Int", `query($n: Int!) { repository(owner: "a", name: "b") { issue(number: $n) { id } } }`, map[string]any{"n": 1.5}, "Variable $n of type Int! was provided invalid value"},
		{"input object field missing", `mutation($input: CreateIssueInput!) { createIssue(input: $input) { issue { id } } }`, map[string]any{"input": map[string]any{"repositoryId": r.NodeID}},
			"Variable $input of type CreateIssueInput! was provided invalid value for title (Expected value to not be null)"},
		{"enum variable with an unknown value", `mutation($input: MergePullRequestInput!) { mergePullRequest(input: $input) { pullRequest { id } } }`, map[string]any{"input": map[string]any{"pullRequestId": "x", "mergeMethod": "FAST"}},
			`Variable $input of type MergePullRequestInput! was provided invalid value for mergeMethod (Expected "FAST" to be one of: MERGE, REBASE, SQUASH)`},
	}
	for _, c := range cases {
		got := h.failing(c.query, c.vars)
		if !contains(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
	// Nothing of an invalid document runs: the valid mutation next to an unknown field did not.
	h.failing(`mutation($id: ID!) { createIssue(input: {repositoryId: $id, title: "must not exist"}) { issue { id } } nope }`, map[string]any{"id": r.NodeID})
	if len(r.Issues) != 0 {
		t.Errorf("a mutation of an invalid document ran: %d issues", len(r.Issues))
	}
	// Operation selection.
	raw, _ := json.Marshal(map[string]any{"query": `query A { viewer { login } } query B { viewer { name } }`})
	if _, reply := h.send("POST", testToken, string(raw)); string(reply) != `{"errors":[{"message":"An operation name is required"}]}` {
		t.Errorf("ambiguous operation: %s", reply)
	}
	raw, _ = json.Marshal(map[string]any{"query": `query A { viewer { login } }`, "operationName": "C"})
	if _, reply := h.send("POST", testToken, string(raw)); string(reply) != `{"errors":[{"message":"No operation named \"C\""}]}` {
		t.Errorf("unknown operation: %s", reply)
	}
}

func TestExecutionErrors(t *testing.T) {
	h := newHarness(t)
	r := must[*store.Repo](t)(h.st.CreateRepo(store.NewRepo{Name: "errors"}))
	must[*store.Issue](t)(r.CreateIssue(store.NewIssue{Title: "one"}))
	push(t, r, "", "a.txt", "first\n")
	push(t, r, "topic", "b.txt", "second\n")

	check := func(name, query string, vars map[string]any, want string) {
		t.Helper()
		_, reply := h.send("POST", testToken, requestBody(query, vars))
		if string(reply) != want {
			t.Errorf("%s:\n got %s\nwant %s", name, reply, want)
		}
	}
	check("unknown node", `{ node(id: "xyz") { id } viewer { login } }`, nil,
		`{"data":{"node":null,"viewer":{"login":"adaiversen"}},"errors":[{"type":"NOT_FOUND","path":["node"],"locations":[{"line":1,"column":3}],"message":"Could not resolve to a node with the global id of 'xyz'"}]}`)
	check("nodes with an unknown id", `query($ids: [ID!]!) { nodes(ids: $ids) { id } }`, map[string]any{"ids": []string{store.ViewerNodeID, "I_nope"}},
		`{"data":{"nodes":[{"id":"`+store.ViewerNodeID+`"},null]},"errors":[{"type":"NOT_FOUND","path":["nodes",1],"message":"Could not resolve to a node with the global id of 'I_nope'"}]}`)
	check("bad cursor", `{ repository(owner: "adaiversen", name: "errors") { name issues(first: 1, after: "xyz") { totalCount } } }`, nil,
		`{"data":{"repository":null},"errors":[{"type":"INVALID_CURSOR_ARGUMENTS","path":["repository","issues"],"locations":[{"line":1,"column":58}],"message":"`+"`xyz`"+` does not appear to be a valid cursor."}]}`)
	check("mutation on the wrong kind of node", `mutation($id: ID!) { closeIssue(input: {issueId: $id}) { issue { id } } }`, map[string]any{"id": r.NodeID},
		`{"data":{"closeIssue":null},"errors":[{"type":"NOT_FOUND","path":["closeIssue"],"locations":[{"line":1,"column":22}],"message":"Could not resolve to a node with the global id of '`+r.NodeID+`'"}]}`)
	check("store validation error", `mutation($id: ID!) { createIssue(input: {repositoryId: $id, title: ""}) { issue { id } } }`, map[string]any{"id": r.NodeID},
		`{"data":{"createIssue":null},"errors":[{"type":"UNPROCESSABLE","path":["createIssue"],"locations":[{"line":1,"column":22}],"message":"Validation Failed"}]}`)
	check("pull request for a missing branch", `mutation($id: ID!) { createPullRequest(input: {repositoryId: $id, title: "x", headRefName: "nope", baseRefName: "main"}) { pullRequest { id } } }`, map[string]any{"id": r.NodeID},
		`{"data":{"createPullRequest":null},"errors":[{"type":"UNPROCESSABLE","path":["createPullRequest"],"locations":[{"line":1,"column":22}],"message":"Validation Failed"}]}`)

	// Mutations run in order; the second one's failure leaves the first one's result in place.
	p := must[*store.Pull](t)(r.CreatePull(store.NewPull{Title: "topic", Head: "topic", Base: "main"}))
	pullID := store.NodeID(store.KindPull, p.ID)
	check("merge twice", `mutation($id: ID!) { first: mergePullRequest(input: {pullRequestId: $id}) { pullRequest { state merged mergeable } } second: mergePullRequest(input: {pullRequestId: $id}) { pullRequest { state } } }`, map[string]any{"id": pullID},
		`{"data":{"first":{"pullRequest":{"state":"MERGED","merged":true,"mergeable":"UNKNOWN"}},"second":null},"errors":[{"type":"UNPROCESSABLE","path":["second"],"locations":[{"line":1,"column":118}],"message":"Pull Request is not mergeable"}]}`)
	if tip, _ := r.Branch("main"); len(r.Objects.GetCommit(tip).Commit.Parents) != 2 {
		t.Errorf("the default merge method did not make a merge commit")
	}
}
