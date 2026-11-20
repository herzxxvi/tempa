// Package rest is the REST v3 surface under /api/v3. Handlers are thin: decode the request, call
// one store operation, render the result with internal/render.
//
// This file is the surface's frame: authentication, routing, the fixed error replies and the helpers
// every handler uses. Routes are registered in routes(); the handlers live in the other files of
// this package.
package rest

import (
	"encoding/json"
	"errors"

	"forge/internal/httpd"
	"forge/internal/render"
	"forge/internal/store"
)

// API serves /api/v3.
type API struct {
	st     *store.Store
	router httpd.Router
}

// New returns the REST surface over st.
func New(st *store.Store) *API {
	a := &API{st: st}
	a.routes()
	return a
}

// routes registers every REST route. Patterns are relative to /api/v3; see httpd.Router for the
// syntax. Parameters are read by position with c.Param(i): in every repository route 0 is the owner
// and 1 the repository.
func (a *API) routes() {
	r := &a.router
	r.Handle(httpd.GET, "/", a.getRoot)
	r.Handle(httpd.GET, "/meta", a.getMeta)
	r.Handle(httpd.GET, "/user", a.getUser)
	r.Handle(httpd.GET, "/user/repos", a.listRepos)
	r.Handle(httpd.POST, "/user/repos", a.createRepo)
	r.Handle(httpd.GET, "/users/{user}", a.getUserByLogin)
	r.Handle(httpd.GET, "/users/{user}/repos", a.listUserRepos)

	const repo = "/repos/{owner}/{repo}"
	r.Handle(httpd.GET, repo, a.getRepo)
	r.Handle(httpd.PATCH, repo, a.patchRepo)
	r.Handle(httpd.DELETE, repo, a.deleteRepo)

	r.Handle(httpd.GET, repo+"/branches", a.listBranches)
	r.Handle(httpd.GET, repo+"/branches/{branch...}", a.getBranch)
	r.Handle(httpd.GET, repo+"/git/refs", a.listRefs)
	r.Handle(httpd.POST, repo+"/git/refs", a.createRef)
	r.Handle(httpd.GET, repo+"/git/refs/{ref...}", a.getRefs)
	r.Handle(httpd.PATCH, repo+"/git/refs/{ref...}", a.patchRef)
	r.Handle(httpd.DELETE, repo+"/git/refs/{ref...}", a.deleteRef)
	r.Handle(httpd.GET, repo+"/git/ref/{ref...}", a.getRef)
	r.Handle(httpd.GET, repo+"/git/matching-refs/{ref...}", a.matchRefs)
	r.Handle(httpd.GET, repo+"/commits", a.listCommits)
	r.Handle(httpd.GET, repo+"/commits/{ref...}", a.getCommit)

	r.Handle(httpd.GET, repo+"/contents", a.getContents)
	r.Handle(httpd.GET, repo+"/contents/{path...}", a.getContents)
	r.Handle(httpd.PUT, repo+"/contents/{path...}", a.putContents)
	r.Handle(httpd.DELETE, repo+"/contents/{path...}", a.deleteContents)

	r.Handle(httpd.GET, repo+"/labels", a.listLabels)
	r.Handle(httpd.POST, repo+"/labels", a.createLabel)
	r.Handle(httpd.GET, repo+"/labels/{name}", a.getLabel)
	r.Handle(httpd.PATCH, repo+"/labels/{name}", a.patchLabel)
	r.Handle(httpd.DELETE, repo+"/labels/{name}", a.deleteLabel)

	r.Handle(httpd.GET, repo+"/issues", a.listIssues)
	r.Handle(httpd.POST, repo+"/issues", a.createIssue)
	r.Handle(httpd.GET, repo+"/issues/{number}", a.getIssue)
	r.Handle(httpd.PATCH, repo+"/issues/{number}", a.patchIssue)
	r.Handle(httpd.GET, repo+"/issues/{number}/labels", a.listIssueLabels)
	r.Handle(httpd.GET, repo+"/issues/{number}/comments", a.listComments)
	r.Handle(httpd.POST, repo+"/issues/{number}/comments", a.createComment)
	r.Handle(httpd.GET, repo+"/issues/comments/{id}", a.getComment)

	r.Handle(httpd.GET, repo+"/pulls", a.listPulls)
	r.Handle(httpd.POST, repo+"/pulls", a.createPull)
	r.Handle(httpd.GET, repo+"/pulls/{number}", a.getPull)
	r.Handle(httpd.PATCH, repo+"/pulls/{number}", a.patchPull)
	r.Handle(httpd.GET, repo+"/pulls/{number}/files", a.listPullFiles)
	r.Handle(httpd.GET, repo+"/pulls/{number}/merge", a.getPullMerge)
	r.Handle(httpd.PUT, repo+"/pulls/{number}/merge", a.mergePull)
}

// The fixed replies. Every REST error body is a JSON object with a non-empty "message".
var (
	requiresAuth   = render.AppendMessage(nil, "Requires authentication")
	badCredentials = render.AppendMessage(nil, "Bad credentials")
	notFound       = render.AppendMessage(nil, "Not Found")
	badJSON        = render.AppendMessage(nil, "Problems parsing JSON")
	metaBody       = []byte(`{"verifiable_password_authentication":false,"installed_version":"3.14.0"}`)
)

// Serve answers one request under /api/v3; sub is the path after that prefix ("" for the root).
// The store is locked by the caller. The order of evaluation is fixed:
//
//  1. authentication: no Authorization header is a 401 "Requires authentication", a wrong token a
//     401 "Bad credentials", for every route including unknown ones;
//  2. route match: no route for (method, path) is a 404 "Not Found";
//  3. the handler: owner and repository (404), body (400), validation (422).
func (a *API) Serve(c *httpd.Ctx, sub []byte) {
	switch c.Auth() {
	case httpd.AuthNone:
		c.JSON(401, requiresAuth)
		return
	case httpd.AuthBad:
		c.JSON(401, badCredentials)
		return
	}
	h := a.router.Find(c, c.Method, sub)
	if h == nil && c.Method == httpd.HEAD {
		// HEAD is GET without the body; the engine leaves the body out of the reply.
		h = a.router.Find(c, httpd.GET, sub)
	}
	if h == nil {
		c.JSON(404, notFound)
		return
	}
	h(c)
}

// NotFound replies 404 {"message":"Not Found",...}.
func NotFound(c *httpd.Ctx) { c.JSON(404, notFound) }

// Fail replies with the REST form of an error returned by the store: its status and its body as
// render.AppendError shapes it. Anything that is not a *store.Error is a bug and becomes a 500.
func Fail(c *httpd.Ctx, err error) {
	var e *store.Error
	if !errors.As(err, &e) {
		panic(err)
	}
	if e == store.ErrNotFound {
		c.JSON(404, notFound)
		return
	}
	c.Scratch = render.AppendError(c.Scratch[:0], e)
	c.JSON(e.Status, c.Scratch)
}

// Repo resolves the {owner}/{repo} parameters at positions i and i+1 of the matched route. When the
// owner is not the viewer or the repository does not exist it replies 404 and returns nil.
func (a *API) Repo(c *httpd.Ctx, i int) *store.Repo {
	r := a.st.Lookup(c.Param(i), c.Param(i+1))
	if r == nil {
		c.JSON(404, notFound)
	}
	return r
}

// Message replies with a bare {"message": ...} body, for the few failures that are the surface's own
// (an undecodable parameter) rather than the store's.
func Message(c *httpd.Ctx, status int, message string) {
	c.Scratch = render.AppendMessage(c.Scratch[:0], message)
	c.JSON(status, c.Scratch)
}

// skipSpace returns b without its leading JSON whitespace.
func skipSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n') {
		b = b[1:]
	}
	return b
}

// DecodeOptional is Decode for routes whose body may be left out entirely (merge, contents delete
// with query parameters): an empty body leaves v untouched and succeeds.
func DecodeOptional(c *httpd.Ctx, v any) bool {
	if len(skipSpace(c.Body)) == 0 {
		return true
	}
	return Decode(c, v)
}

// Decode parses the request body, which must be a JSON object, into v. On failure it replies 400
// "Problems parsing JSON" (malformed JSON, an empty body, or a value that is not an object) or 422
// (a field of the wrong JSON type) and returns false.
func Decode(c *httpd.Ctx, v any) bool {
	body := skipSpace(c.Body)
	if len(body) == 0 || body[0] != '{' {
		c.JSON(400, badJSON)
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			c.Scratch = render.AppendMessage(c.Scratch[:0], "Invalid request.\n\nFor 'properties/"+typeErr.Field+"', the value is not of the expected type.")
			c.JSON(422, c.Scratch)
			return false
		}
		c.JSON(400, badJSON)
		return false
	}
	return true
}

// getRoot is GET /api/v3 and /api/v3/: the API index, with the token's scopes in X-OAuth-Scopes.
func (a *API) getRoot(c *httpd.Ctx) {
	b := c.Scratch[:0]
	b = append(b, `{"current_user_url":"http://`...)
	b = append(b, c.Host...)
	b = append(b, `/api/v3/user","user_repositories_url":"http://`...)
	b = append(b, c.Host...)
	b = append(b, `/api/v3/user/repos","repository_url":"http://`...)
	b = append(b, c.Host...)
	b = append(b, `/api/v3/repos/{owner}/{repo}"}`...)
	c.Scratch = b
	c.AddHeader("X-OAuth-Scopes", "repo, read:org")
	c.JSON(200, b)
}

// getMeta is GET /api/v3/meta.
func (a *API) getMeta(c *httpd.Ctx) { c.JSON(200, metaBody) }

// getUser is GET /api/v3/user: the viewer.
func (a *API) getUser(c *httpd.Ctx) {
	c.Scratch = render.AppendViewer(c.Scratch[:0], c.Host)
	c.JSON(200, c.Scratch)
}
