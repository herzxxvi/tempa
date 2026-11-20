package rest

import (
	"strings"

	"forge/internal/httpd"
	"forge/internal/render"
	"forge/internal/store"
)

// Labels, issues and issue comments.
//
// Two of these routes are timed by the benchmark: GET /issues/{number} (thousands of reads of one
// issue on one connection) and POST /issues (hundreds of creations). Both reply with the issue's
// pre-rendered tail, which render.IssueTail keeps on the issue until the issue changes; the read
// does not allocate and the creation allocates only what it stores.

// listLabels is GET /labels: the repository's labels in creation order, paginated.
func (a *API) listLabels(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	p := paging(c)
	lo, hi := p.bounds(len(r.Labels))
	c.Scratch = render.AppendLabels(c.Scratch[:0], c.Host, r.Labels[lo:hi])
	p.link(c, len(r.Labels))
	c.JSON(200, c.Scratch)
}

// labelInput is the body of POST /labels and PATCH /labels/{name}.
type labelInput struct {
	Name        *string   `json:"name"`
	NewName     *string   `json:"new_name"`
	Color       *string   `json:"color"`
	Description optString `json:"description"`
}

func replyLabel(c *httpd.Ctx, status int, l *store.Label) {
	c.Scratch = render.AppendLabel(c.Scratch[:0], c.Host, l)
	c.JSON(status, c.Scratch)
}

// createLabel is POST /labels {"name", "color", "description"}: 201; 422 for a missing or taken name.
func (a *API) createLabel(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	var in labelInput
	if !Decode(c, &in) {
		return
	}
	var name, color string
	if in.Name != nil {
		name = *in.Name
	}
	if in.Color != nil {
		color = *in.Color
	}
	l, err := r.CreateLabel(name, color, in.Description.Value, in.Description.Set && !in.Description.Null)
	if err != nil {
		Fail(c, err)
		return
	}
	replyLabel(c, 201, l)
}

// label resolves the {name} parameter of a label route; it replies 404 and returns nil when the
// repository or the label does not exist.
func (a *API) label(c *httpd.Ctx) (*store.Repo, *store.Label) {
	r := a.Repo(c, 0)
	if r == nil {
		return nil, nil
	}
	name, ok := c.ParamString(2)
	if !ok {
		NotFound(c)
		return nil, nil
	}
	l := r.Label(name)
	if l == nil {
		NotFound(c)
	}
	return r, l
}

// getLabel is GET /labels/{name}.
func (a *API) getLabel(c *httpd.Ctx) {
	if _, l := a.label(c); l != nil {
		replyLabel(c, 200, l)
	}
}

// patchLabel is PATCH /labels/{name} {"new_name", "color", "description"}.
func (a *API) patchLabel(c *httpd.Ctx) {
	r, l := a.label(c)
	if l == nil {
		return
	}
	var in labelInput
	if !Decode(c, &in) {
		return
	}
	patch := store.LabelPatch{Name: in.NewName, Color: in.Color, Description: in.Description.ptr()}
	if patch.Name == nil {
		patch.Name = in.Name
	}
	if err := r.UpdateLabel(l, patch); err != nil {
		Fail(c, err)
		return
	}
	replyLabel(c, 200, l)
}

// deleteLabel is DELETE /labels/{name}: 204; the label also leaves every issue that carried it.
func (a *API) deleteLabel(c *httpd.Ctx) {
	r, l := a.label(c)
	if l == nil {
		return
	}
	r.DeleteLabel(l)
	c.Empty(204)
}

// invalidParam is the 422 for a query parameter with a value outside its set.
func invalidParam(c *httpd.Ctx, resource, field, value string) {
	Fail(c, &store.Error{Status: 422, Message: "Validation Failed", Resource: resource, Field: field, Code: "invalid", Value: value})
}

// listOrder reads sort and direction. updated selects SortUpdated, comments SortComments when
// allowed, anything else creation order; the direction is descending unless it says asc.
func listOrder(c *httpd.Ctx, comments bool) (by store.Sort, asc bool) {
	switch v, _ := c.QueryRaw("sort"); string(v) {
	case "updated":
		by = store.SortUpdated
	case "comments":
		if comments {
			by = store.SortComments
		}
	}
	v, _ := c.QueryRaw("direction")
	return by, string(v) == "asc"
}

// listState reads the state filter: open (the default), closed or all. Any other value is a 422;
// ok is false after that reply.
func listState(c *httpd.Ctx, resource string) (state store.State, ok bool) {
	v, found := c.QueryRaw("state")
	if !found || len(v) == 0 {
		return store.StateOpen, true
	}
	if state, ok = store.ParseState(string(v)); !ok {
		invalidParam(c, resource, "state", string(v))
	}
	return state, ok
}

// listIssues is GET /issues[?state=&labels=a,b&sort=&direction=&per_page=&page=]: open issues,
// newest first, unless the query says otherwise. Pull requests are not issues here. Creation order
// is number order, so issues created within one second still sort the way they were made.
func (a *API) listIssues(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	q := store.IssueQuery{}
	var ok bool
	if q.State, ok = listState(c, "Issue"); !ok {
		return
	}
	q.Sort, q.Asc = listOrder(c, true)
	if labels := c.QueryString("labels"); labels != "" {
		for _, name := range strings.Split(labels, ",") {
			if name = strings.TrimSpace(name); name != "" {
				q.Labels = append(q.Labels, name)
			}
		}
	}
	p := paging(c)
	q.Offset, q.Limit = p.offset(), p.size
	var buf [maxPerPage]*store.Issue
	issues, total := r.ListIssues(q, buf[:0])
	c.Scratch = render.AppendIssues(c.Scratch[:0], c.Host, issues)
	p.link(c, total)
	c.JSON(200, c.Scratch)
}

// issueInput is the body of POST /issues and PATCH /issues/{number}.
type issueInput struct {
	Title       *string    `json:"title"`
	Body        optString  `json:"body"`
	State       *string    `json:"state"`
	StateReason optString  `json:"state_reason"`
	Labels      labelNames `json:"labels"`
}

// createIssue is POST /issues {"title", "body", "labels"}: 201 with the issue. Labels must already
// exist: an unknown name is a 422, never a new label.
func (a *API) createIssue(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	var n store.NewIssue
	if title, body, ok := scanIssue(c.Body); ok {
		// One copy out of the request buffer holds both strings.
		var text strings.Builder
		text.Grow(len(title) + len(body))
		text.Write(title)
		text.Write(body)
		n.Title, n.Body = text.String()[:len(title)], text.String()[len(title):]
	} else {
		var in issueInput
		if !Decode(c, &in) {
			return
		}
		if in.Title != nil {
			n.Title = *in.Title
		}
		n.Body, n.Labels = in.Body.Value, in.Labels.Names
	}
	is, err := r.CreateIssue(n)
	if err != nil {
		Fail(c, err)
		return
	}
	c.JSONTail(201, render.IssueTail(c.Host, is))
}

// issue resolves the {number} parameter of an issue route; it replies 404 and returns nil when the
// repository or the issue does not exist (a pull request's number is not an issue).
func (a *API) issue(c *httpd.Ctx) (*store.Repo, *store.Issue) {
	r := a.Repo(c, 0)
	if r == nil {
		return nil, nil
	}
	n, ok := c.ParamInt(2)
	if !ok {
		NotFound(c)
		return nil, nil
	}
	is := r.Issue(n)
	if is == nil {
		NotFound(c)
	}
	return r, is
}

// getIssue is GET /issues/{number}, the read benchmark's route: two lookups and the cached tail.
func (a *API) getIssue(c *httpd.Ctx) {
	if _, is := a.issue(c); is != nil {
		c.JSONTail(200, render.IssueTail(c.Host, is))
	}
}

// patchIssue is PATCH /issues/{number}: title, body (null empties it), state open or closed,
// state_reason, labels (the new complete set).
func (a *API) patchIssue(c *httpd.Ctx) {
	r, is := a.issue(c)
	if is == nil {
		return
	}
	var in issueInput
	if !Decode(c, &in) {
		return
	}
	patch := store.IssuePatch{Title: in.Title, Body: in.Body.ptr(), State: in.State}
	if in.StateReason.Set && !in.StateReason.Null {
		patch.StateReason = &in.StateReason.Value
	}
	if in.Labels.Set {
		patch.Labels = &in.Labels.Names
	}
	if err := r.UpdateIssue(is, patch); err != nil {
		Fail(c, err)
		return
	}
	c.JSONTail(200, render.IssueTail(c.Host, is))
}

// listIssueLabels is GET /issues/{number}/labels.
func (a *API) listIssueLabels(c *httpd.Ctx) {
	if _, is := a.issue(c); is != nil {
		c.Scratch = render.AppendLabels(c.Scratch[:0], c.Host, is.Labels)
		c.JSON(200, c.Scratch)
	}
}

// number reads the {number} parameter of a route that takes an issue or a pull request number.
func (a *API) number(c *httpd.Ctx) (r *store.Repo, n int64, ok bool) {
	if r = a.Repo(c, 0); r == nil {
		return nil, 0, false
	}
	if n, ok = c.ParamInt(2); !ok {
		NotFound(c)
	}
	return r, n, ok
}

// listComments is GET /issues/{number}/comments: the comments of an issue or a pull request in
// creation order, paginated.
func (a *API) listComments(c *httpd.Ctx) {
	r, n, ok := a.number(c)
	if !ok {
		return
	}
	comments, ok := r.Comments(n)
	if !ok {
		NotFound(c)
		return
	}
	p := paging(c)
	lo, hi := p.bounds(len(comments))
	c.Scratch = render.AppendComments(c.Scratch[:0], c.Host, comments[lo:hi])
	p.link(c, len(comments))
	c.JSON(200, c.Scratch)
}

// createComment is POST /issues/{number}/comments {"body"}: 201 with the comment.
func (a *API) createComment(c *httpd.Ctx) {
	r, n, ok := a.number(c)
	if !ok {
		return
	}
	if _, ok := r.Comments(n); !ok {
		NotFound(c)
		return
	}
	var in struct {
		Body optString `json:"body"`
	}
	if !Decode(c, &in) {
		return
	}
	comment, err := r.AddComment(n, in.Body.Value)
	if err != nil {
		Fail(c, err)
		return
	}
	c.Scratch = render.AppendComment(c.Scratch[:0], c.Host, comment)
	c.JSON(201, c.Scratch)
}

// getComment is GET /issues/comments/{id}: one comment by its id.
func (a *API) getComment(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	id, ok := c.ParamInt(2)
	comment := a.st.CommentByID(id)
	if !ok || comment == nil || comment.Repo != r {
		NotFound(c)
		return
	}
	c.Scratch = render.AppendComment(c.Scratch[:0], c.Host, comment)
	c.JSON(200, c.Scratch)
}
