package rest

import (
	"forge/internal/httpd"
	"forge/internal/render"
	"forge/internal/store"
)

// Pull requests. Everything that depends on git state (head and base sha, the changed files,
// whether it merges) is computed by the store from the current refs at the time of the request.

// listPulls is GET /pulls[?state=&head=&base=&sort=&direction=&per_page=&page=]: open pull requests,
// newest first, unless the query says otherwise. state=closed includes merged ones.
func (a *API) listPulls(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	q := store.PullQuery{Head: c.QueryString("head"), Base: c.QueryString("base")}
	var ok bool
	if q.State, ok = listState(c, "PullRequest"); !ok {
		return
	}
	q.Sort, q.Asc = listOrder(c, false)
	p := paging(c)
	q.Offset, q.Limit = p.offset(), p.size
	var buf [maxPerPage]*store.Pull
	pulls, total := r.ListPulls(q, buf[:0])
	c.Scratch = render.AppendPulls(c.Scratch[:0], c.Host, pulls)
	p.link(c, total)
	c.JSON(200, c.Scratch)
}

// pullInput is the body of POST /pulls and PATCH /pulls/{number}.
type pullInput struct {
	Title *string   `json:"title"`
	Body  optString `json:"body"`
	Head  string    `json:"head"`
	Base  string    `json:"base"`
	Draft bool      `json:"draft"`
	State *string   `json:"state"`
}

func replyPull(c *httpd.Ctx, status int, p *store.Pull) {
	c.Scratch = render.AppendPull(c.Scratch[:0], c.Host, p)
	c.JSON(status, c.Scratch)
}

// createPull is POST /pulls {"title", "head", "base", "body", "draft"}: 201 with the pull request.
// Both branches must exist; a second open pull request for the same pair is a 422.
func (a *API) createPull(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	var in pullInput
	if !Decode(c, &in) {
		return
	}
	n := store.NewPull{Body: in.Body.Value, Head: in.Head, Base: in.Base, Draft: in.Draft}
	if in.Title != nil {
		n.Title = *in.Title
	}
	p, err := r.CreatePull(n)
	if err != nil {
		Fail(c, err)
		return
	}
	replyPull(c, 201, p)
}

// pull resolves the {number} parameter of a pull request route; it replies 404 and returns nil when
// the repository or the pull request does not exist (an issue's number is not a pull request).
func (a *API) pull(c *httpd.Ctx) (*store.Repo, *store.Pull) {
	r, n, ok := a.number(c)
	if !ok {
		return nil, nil
	}
	p := r.Pull(n)
	if p == nil {
		NotFound(c)
	}
	return r, p
}

// getPull is GET /pulls/{number}.
func (a *API) getPull(c *httpd.Ctx) {
	if _, p := a.pull(c); p != nil {
		replyPull(c, 200, p)
	}
}

// patchPull is PATCH /pulls/{number}: title, body, state open or closed.
func (a *API) patchPull(c *httpd.Ctx) {
	r, p := a.pull(c)
	if p == nil {
		return
	}
	var in pullInput
	if !Decode(c, &in) {
		return
	}
	if err := r.UpdatePull(p, store.PullPatch{Title: in.Title, Body: in.Body.ptr(), State: in.State}); err != nil {
		Fail(c, err)
		return
	}
	replyPull(c, 200, p)
}

// listPullFiles is GET /pulls/{number}/files: the files that differ between the merge base and the
// head, sorted by path, paginated.
func (a *API) listPullFiles(c *httpd.Ctx) {
	r, p := a.pull(c)
	if p == nil {
		return
	}
	info := r.PullInfo(p)
	pg := paging(c)
	lo, hi := pg.bounds(len(info.Files))
	c.Scratch = render.AppendPullFiles(c.Scratch[:0], c.Host, r, info, info.Files[lo:hi])
	pg.link(c, len(info.Files))
	c.JSON(200, c.Scratch)
}

// getPullMerge is GET /pulls/{number}/merge: 204 when the pull request has been merged, else 404.
func (a *API) getPullMerge(c *httpd.Ctx) {
	_, p := a.pull(c)
	if p == nil {
		return
	}
	if !p.Merged {
		NotFound(c)
		return
	}
	c.Empty(204)
}

// mergePull is PUT /pulls/{number}/merge {"merge_method", "commit_title", "commit_message", "sha"}:
// 200 {"sha", "merged": true, "message"}. The body is optional. A pull request that is closed,
// already merged or in conflict with its base as the base is now is the 405 "Pull Request is not
// mergeable"; nothing changes then.
func (a *API) mergePull(c *httpd.Ctx) {
	r, p := a.pull(c)
	if p == nil {
		return
	}
	var in struct {
		Method        string `json:"merge_method"`
		CommitTitle   string `json:"commit_title"`
		CommitMessage string `json:"commit_message"`
		SHA           string `json:"sha"`
	}
	if !DecodeOptional(c, &in) {
		return
	}
	commit, err := r.MergePull(p, store.MergeOptions{Method: in.Method, CommitTitle: in.CommitTitle, CommitMessage: in.CommitMessage, SHA: in.SHA})
	if err != nil {
		Fail(c, err)
		return
	}
	c.Scratch = render.AppendMergeResult(c.Scratch[:0], commit.ID)
	c.JSON(200, c.Scratch)
}
