package rest

import (
	"sort"

	"forge/internal/httpd"
	"forge/internal/render"
	"forge/internal/store"
)

// repoInput is the body of POST /user/repos and PATCH /repos/{owner}/{repo}.
type repoInput struct {
	Name          *string   `json:"name"`
	Description   optString `json:"description"`
	Private       *bool     `json:"private"`
	Visibility    *string   `json:"visibility"`
	HasIssues     *bool     `json:"has_issues"`
	Archived      *bool     `json:"archived"`
	DefaultBranch *string   `json:"default_branch"`
	AutoInit      bool      `json:"auto_init"`
}

// private reports what the body says about visibility: "private" wins, then "visibility".
func (in *repoInput) private() *bool {
	if in.Private != nil {
		return in.Private
	}
	if in.Visibility != nil {
		p := *in.Visibility != "public"
		return &p
	}
	return nil
}

// replyRepo answers with a repository object.
func replyRepo(c *httpd.Ctx, status int, r *store.Repo) {
	c.Scratch = render.AppendRepo(c.Scratch[:0], c.Host, r)
	c.JSON(status, c.Scratch)
}

// createRepo is POST /user/repos: 201 with the new, empty repository. A taken name is the 422 whose
// body says "name already exists on this account". With auto_init the repository starts with a
// README.md commit on main, otherwise it has no commits and no branches.
func (a *API) createRepo(c *httpd.Ctx) {
	var in repoInput
	if !Decode(c, &in) {
		return
	}
	n := store.NewRepo{Description: in.Description.Value, HasDescription: in.Description.Set && !in.Description.Null}
	if in.Name != nil {
		n.Name = *in.Name
	}
	if p := in.private(); p != nil {
		n.Private = *p
	}
	if in.HasIssues != nil {
		n.NoIssues = !*in.HasIssues
	}
	r, err := a.st.CreateRepo(n)
	if err != nil {
		Fail(c, err)
		return
	}
	if in.AutoInit {
		readme := "# " + r.Name + "\n"
		if r.Description != "" {
			readme += "\n" + r.Description + "\n"
		}
		if _, err := r.PutFile(store.FileWrite{Path: "README.md", Message: "Initial commit", Data: []byte(readme)}); err != nil {
			Fail(c, err)
			return
		}
	}
	replyRepo(c, 201, r)
}

// getRepo is GET /repos/{owner}/{repo}.
func (a *API) getRepo(c *httpd.Ctx) {
	if r := a.Repo(c, 0); r != nil {
		replyRepo(c, 200, r)
	}
}

// patchRepo is PATCH /repos/{owner}/{repo}: description (null clears it), private or visibility,
// has_issues, archived, default_branch and name (a rename).
func (a *API) patchRepo(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	var in repoInput
	if !Decode(c, &in) {
		return
	}
	patch := store.RepoPatch{
		Name: in.Name, Private: in.private(), HasIssues: in.HasIssues, Archived: in.Archived, DefaultBranch: in.DefaultBranch,
	}
	if in.Description.Null {
		patch.ClearDescription = true
	} else {
		patch.Description = in.Description.ptr()
	}
	if err := a.st.UpdateRepo(r, patch); err != nil {
		Fail(c, err)
		return
	}
	replyRepo(c, 200, r)
}

// deleteRepo is DELETE /repos/{owner}/{repo}: 204, and the repository is gone from every surface.
func (a *API) deleteRepo(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	a.st.DeleteRepo(r)
	c.Empty(204)
}

// listRepos is GET /user/repos: the viewer's repositories, paginated.
//
// Order: creation order (oldest first) unless the request names one with sort = created, updated,
// pushed or full_name; direction defaults to asc for full_name and desc for the others, as on GitHub.
// visibility (or type) = public or private filters.
func (a *API) listRepos(c *httpd.Ctx) {
	repos := a.st.Repos()
	filter := c.QueryString("visibility")
	if filter == "" {
		filter = c.QueryString("type")
	}
	by := c.QueryString("sort")
	if filter == "public" || filter == "private" || by != "" {
		selected := make([]*store.Repo, 0, len(repos))
		for _, r := range repos {
			if (filter == "public" && r.Private) || (filter == "private" && !r.Private) {
				continue
			}
			selected = append(selected, r)
		}
		repos = selected
		sortRepos(repos, by, c.QueryString("direction"))
	}
	p := paging(c)
	lo, hi := p.bounds(len(repos))
	b := append(c.Scratch[:0], '[')
	for i, r := range repos[lo:hi] {
		if i > 0 {
			b = append(b, ',')
		}
		b = render.AppendRepo(b, c.Host, r)
	}
	c.Scratch = append(b, ']')
	p.link(c, len(repos))
	c.JSON(200, c.Scratch)
}

// sortRepos orders repos (which arrive in creation order) by the named key. Ties keep creation
// order in the direction of the sort, so the result never depends on anything but the data.
func sortRepos(repos []*store.Repo, by, direction string) {
	var key func(r *store.Repo) int64
	switch by {
	case "created":
		key = func(r *store.Repo) int64 { return r.ID }
	case "updated":
		key = func(r *store.Repo) int64 { return r.UpdatedAt }
	case "pushed":
		key = func(r *store.Repo) int64 { return r.PushedAt }
	case "full_name":
	default:
		return
	}
	asc := direction == "asc" || (direction != "desc" && by == "full_name")
	sort.SliceStable(repos, func(i, j int) bool {
		x, y := repos[i], repos[j]
		if key == nil {
			if x.Name != y.Name {
				return (x.Name < y.Name) == asc
			}
		} else if kx, ky := key(x), key(y); kx != ky {
			return (kx < ky) == asc
		}
		return (x.ID < y.ID) == asc
	})
}

// getUserByLogin is GET /users/{user}: the viewer is the only user there is.
func (a *API) getUserByLogin(c *httpd.Ctx) {
	if !store.IsViewer(c.Param(0)) {
		NotFound(c)
		return
	}
	a.getUser(c)
}

// listUserRepos is GET /users/{user}/repos: the same list as GET /user/repos.
func (a *API) listUserRepos(c *httpd.Ctx) {
	if !store.IsViewer(c.Param(0)) {
		NotFound(c)
		return
	}
	a.listRepos(c)
}
