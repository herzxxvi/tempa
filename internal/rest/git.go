package rest

import (
	"strings"

	"forge/internal/gitobj"
	"forge/internal/httpd"
	"forge/internal/render"
	"forge/internal/store"
)

// Branches, git refs and commits: read views of the repository's refs and history, and the ref
// writes. Branch and ref names may contain slashes and may arrive percent-encoded, so their routes
// capture the rest of the path and decode it.

// restParam returns the decoded catch-all parameter at position i, without surrounding slashes.
// When it is not decodable it replies 404 and returns ok false.
func restParam(c *httpd.Ctx, i int) (s string, ok bool) {
	s, ok = c.ParamString(i)
	if !ok {
		NotFound(c)
		return "", false
	}
	return strings.Trim(s, "/"), true
}

// listBranches is GET /branches: every branch sorted by name, paginated.
func (a *API) listBranches(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	branches := r.Branches()
	p := paging(c)
	lo, hi := p.bounds(len(branches))
	b := append(c.Scratch[:0], '[')
	for i, ref := range branches[lo:hi] {
		if i > 0 {
			b = append(b, ',')
		}
		b = render.AppendBranch(b, c.Host, r, ref)
	}
	c.Scratch = append(b, ']')
	p.link(c, len(branches))
	c.JSON(200, c.Scratch)
}

// getBranch is GET /branches/{branch}: 404 "Branch not found" for a branch that does not exist.
func (a *API) getBranch(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	name, ok := restParam(c, 2)
	if !ok {
		return
	}
	id, ok := r.Branch(name)
	if !ok {
		Message(c, 404, "Branch not found")
		return
	}
	c.Scratch = render.AppendBranch(c.Scratch[:0], c.Host, r, store.Ref{Name: "refs/heads/" + name, ID: id})
	c.JSON(200, c.Scratch)
}

// replyRefs answers with an array of git ref objects.
func replyRefs(c *httpd.Ctx, r *store.Repo, refs []store.Ref) {
	p := paging(c)
	lo, hi := p.bounds(len(refs))
	b := append(c.Scratch[:0], '[')
	for i, ref := range refs[lo:hi] {
		if i > 0 {
			b = append(b, ',')
		}
		b = render.AppendGitRef(b, c.Host, r, ref)
	}
	c.Scratch = append(b, ']')
	p.link(c, len(refs))
	c.JSON(200, c.Scratch)
}

// matching returns the refs whose full name starts with prefix; they are contiguous in the sorted
// ref list.
func matching(r *store.Repo, prefix string) []store.Ref {
	refs := r.Refs()
	lo := 0
	for lo < len(refs) && !strings.HasPrefix(refs[lo].Name, prefix) {
		lo++
	}
	hi := lo
	for hi < len(refs) && strings.HasPrefix(refs[hi].Name, prefix) {
		hi++
	}
	return refs[lo:hi]
}

// listRefs is GET /git/refs: every ref, sorted by name.
func (a *API) listRefs(c *httpd.Ctx) {
	if r := a.Repo(c, 0); r != nil {
		replyRefs(c, r, r.Refs())
	}
}

// refParam resolves the {ref...} parameter ("heads/main") to a full ref name ("refs/heads/main").
func refParam(c *httpd.Ctx) (full string, ok bool) {
	name, ok := restParam(c, 2)
	return "refs/" + name, ok
}

// getRef is GET /git/ref/{ref}: the one ref with exactly that name, or 404.
func (a *API) getRef(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	full, ok := refParam(c)
	if !ok {
		return
	}
	id, ok := r.Ref(full)
	if !ok {
		NotFound(c)
		return
	}
	c.Scratch = render.AppendGitRef(c.Scratch[:0], c.Host, r, store.Ref{Name: full, ID: id})
	c.JSON(200, c.Scratch)
}

// getRefs is GET /git/refs/{ref}: the ref with exactly that name as an object; otherwise the refs
// under that prefix as an array ("/git/refs/heads" lists the branches); 404 when nothing matches.
func (a *API) getRefs(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	full, ok := refParam(c)
	if !ok {
		return
	}
	if id, ok := r.Ref(full); ok {
		c.Scratch = render.AppendGitRef(c.Scratch[:0], c.Host, r, store.Ref{Name: full, ID: id})
		c.JSON(200, c.Scratch)
		return
	}
	refs := matching(r, full+"/")
	if len(refs) == 0 {
		NotFound(c)
		return
	}
	replyRefs(c, r, refs)
}

// matchRefs is GET /git/matching-refs/{ref}: the refs whose name starts with the given text, as an
// array that may be empty.
func (a *API) matchRefs(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	if full, ok := refParam(c); ok {
		replyRefs(c, r, matching(r, full))
	}
}

// createRef is POST /git/refs {"ref": "refs/heads/x", "sha": "<40 hex>"}: 201 with the ref. A sha
// that is not an object of this repository is the 422 "Object does not exist".
func (a *API) createRef(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	var in struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if !Decode(c, &in) {
		return
	}
	id, _ := gitobj.ParseOID(in.SHA) // a malformed sha is the zero id, which no repository has
	if err := r.CreateRef(in.Ref, id); err != nil {
		Fail(c, err)
		return
	}
	c.Scratch = render.AppendGitRef(c.Scratch[:0], c.Host, r, store.Ref{Name: in.Ref, ID: id})
	c.JSON(201, c.Scratch)
}

// patchRef is PATCH /git/refs/{ref} {"sha": ..., "force": bool}: move a ref; without force only
// forward.
func (a *API) patchRef(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	full, ok := refParam(c)
	if !ok {
		return
	}
	var in struct {
		SHA   string `json:"sha"`
		Force bool   `json:"force"`
	}
	if !Decode(c, &in) {
		return
	}
	id, _ := gitobj.ParseOID(in.SHA)
	if err := r.UpdateRef(full, id, in.Force); err != nil {
		Fail(c, err)
		return
	}
	c.Scratch = render.AppendGitRef(c.Scratch[:0], c.Host, r, store.Ref{Name: full, ID: id})
	c.JSON(200, c.Scratch)
}

// deleteRef is DELETE /git/refs/{ref}: 204.
func (a *API) deleteRef(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	full, ok := refParam(c)
	if !ok {
		return
	}
	if err := r.DeleteRef(full); err != nil {
		Fail(c, err)
		return
	}
	c.Empty(204)
}

// listCommits is GET /commits[?sha=<branch, tag or commit>]: history newest first, paginated.
// Without sha it is the default branch's history only. An empty repository is the 409 "Git
// Repository is empty.", an unknown sha the 404 "No commit found for SHA: <sha>".
func (a *API) listCommits(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	p := paging(c)
	var buf [maxPerPage]*gitobj.Object
	commits, more, err := r.Log(c.QueryString("sha"), p.offset(), p.size, buf[:0])
	if err != nil {
		Fail(c, err)
		return
	}
	b := append(c.Scratch[:0], '[')
	for i, o := range commits {
		if i > 0 {
			b = append(b, ',')
		}
		b = render.AppendCommit(b, c.Host, r, o)
	}
	c.Scratch = append(b, ']')
	p.linkMore(c, more)
	c.JSON(200, c.Scratch)
}

// getCommit is GET /commits/{ref}: one commit named by a branch, a tag or a commit id.
func (a *API) getCommit(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	rev, ok := restParam(c, 2)
	if !ok {
		return
	}
	// Log applies the rules for "empty" and "unknown" and reports them the way the list does.
	var buf [1]*gitobj.Object
	commits, _, err := r.Log(rev, 0, 1, buf[:0])
	if err != nil {
		Fail(c, err)
		return
	}
	c.Scratch = render.AppendCommit(c.Scratch[:0], c.Host, r, commits[0])
	c.JSON(200, c.Scratch)
}
