package graphql

import (
	"cmp"
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
	"time"

	"forge/internal/gitobj"
	"forge/internal/render"
	"forge/internal/store"
)

// The resolvers of schema.graphql. Each reads the live store objects that REST renders, and each
// mutation calls the store operation the matching REST route calls, so the surfaces cannot drift
// apart. The Go values behind the object types are:
//
//	User          store.Viewer        Repository    *store.Repo
//	Issue         *store.Issue        PullRequest   *store.Pull
//	IssueComment  *store.Comment      Label         *store.Label
//	Ref           *ref                Commit        *commit
//	PullRequestChangedFile  *store.FileChange
//	Query, Mutation  root             *Payload      *payload

// root is the value the fields of Query and Mutation are resolved on.
type root struct{}

// ref is a Git reference, by its full name ("refs/heads/main").
type ref struct {
	repo *store.Repo
	name string
}

// commit is a commit of a repository.
type commit struct {
	repo *store.Repo
	obj  *gitobj.Object
}

// payload is the value of every mutation payload type.
type payload struct {
	mutationID any // the input's clientMutationId, echoed
	issue      any
	pull       any
	edge       any
	subject    any
}

// ---- helpers ----

func (q *query) origin() string { return "http://" + q.host }

func (q *query) repoURL(r *store.Repo) string { return q.origin() + "/" + r.FullName() }

// stamp formats a store time; the zero time is null.
func stamp(t int64) any {
	if t == 0 {
		return nil
	}
	return string(render.AppendTime(nil, t))
}

// text is a string that is null when empty.
func text(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// gitNodeID is the node id of an object that lives in a repository's git data rather than in the
// store's tables: "<prefix>_<base64url of 'Kind:<repository id>:<name>'>".
func gitNodeID(prefix, kind string, r *store.Repo, name string) string {
	return prefix + "_" + base64.RawURLEncoding.EncodeToString([]byte(kind+":"+strconv.FormatInt(r.ID, 10)+":"+name))
}

// node resolves a global id to the value of the object it names, or nil.
func (q *query) node(id string) any {
	if n := q.st.Node(id); n != nil {
		return n
	}
	prefix, enc, _ := strings.Cut(id, "_")
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	parts := strings.SplitN(string(raw), ":", 3)
	if err != nil || len(parts) != 3 {
		return nil
	}
	repoID, err := strconv.ParseInt(parts[1], 10, 64)
	r := q.st.RepoByID(repoID)
	if err != nil || r == nil {
		return nil
	}
	switch prefix + ":" + parts[0] {
	case "REF:Ref":
		if _, ok := r.Ref(parts[2]); ok {
			return &ref{r, parts[2]}
		}
	case "C:Commit":
		if oid, ok := gitobj.ParseOID(parts[2]); ok {
			if o := r.Objects.GetCommit(oid); o != nil {
				return &commit{r, o}
			}
		}
	}
	return nil
}

// branch returns the Ref of a branch, or nil when the branch does not exist.
func branch(r *store.Repo, short string) any {
	if _, ok := r.Branch(short); !ok {
		return nil
	}
	return &ref{r, "refs/heads/" + short}
}

// order reads an IssueOrder argument; without one the order is creation order, oldest first.
func order(a args) (by store.Sort, asc bool) {
	o := a.object("orderBy")
	if o == nil {
		return store.SortCreated, true
	}
	switch o.str("field") {
	case "UPDATED_AT":
		by = store.SortUpdated
	case "COMMENTS":
		by = store.SortComments
	}
	return by, o.str("direction") != "DESC"
}

// compare is the order of an issue or pull request listing: the sort key, then the number, both in
// the listing's direction. It is the order store.ListIssues produces.
func compare(by store.Sort, asc bool) func(a, b any) int {
	key := func(v any) (key, number int64) {
		switch x := v.(type) {
		case *store.Issue:
			key, number = x.UpdatedAt, x.Number
			if by == store.SortComments {
				key = int64(len(x.Comments))
			}
		case *store.Pull:
			key, number = x.UpdatedAt, x.Number
			if by == store.SortComments {
				key = int64(len(x.Comments))
			}
		}
		if by == store.SortCreated {
			key = 0 // numbers are handed out in creation order
		}
		return key, number
	}
	return func(a, b any) int {
		ka, na := key(a)
		kb, nb := key(b)
		c := cmp.Compare(ka, kb)
		if c == 0 {
			c = cmp.Compare(na, nb)
		}
		if !asc {
			c = -c
		}
		return c
	}
}

func idKey(id int64) string { return strconv.FormatInt(id, 10) }

func keyID(key string) int64 {
	id, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// mentions reports whether the issue's text mentions @login.
func mentions(is *store.Issue, login string) bool {
	needle := "@" + strings.ToLower(login)
	if strings.Contains(strings.ToLower(is.Body), needle) {
		return true
	}
	for _, c := range is.Comments {
		if strings.Contains(strings.ToLower(c.Body), needle) {
			return true
		}
	}
	return false
}

// issues is Repository.issues.
func (q *query) issues(r *store.Repo, a args) (any, error) {
	by, asc := order(a)
	filter := a.object("filterBy")
	states, names := a.strings("states"), a.strings("labels")
	if !a.has("states") {
		states = filter.strings("states")
	}
	if !a.has("labels") {
		names = filter.strings("labels")
	}
	open, closed := false, false
	for _, s := range states {
		open = open || s == "OPEN"
		closed = closed || s == "CLOSED"
	}
	sel := store.IssueQuery{State: store.StateAll, Labels: names, Sort: by, Asc: asc}
	switch {
	case open && !closed:
		sel.State = store.StateOpen
	case closed && !open:
		sel.State = store.StateClosed
	}
	var since int64
	if filter.has("since") {
		t, err := time.Parse(time.RFC3339, filter.str("since"))
		if err != nil {
			return nil, &Error{Type: "UNPROCESSABLE", Message: "since is not an ISO-8601 date and time: " + filter.str("since")}
		}
		since = t.Unix()
	}
	found, _ := r.ListIssues(sel, nil)
	list := make([]any, 0, len(found))
	for _, is := range found {
		switch {
		case is.UpdatedAt < since:
		case filter.has("createdBy") && !strings.EqualFold(filter.str("createdBy"), store.ViewerLogin):
		case filter.has("assignee"): // nobody is ever assigned
		case filter.has("mentioned") && !mentions(is, filter.str("mentioned")):
		default:
			list = append(list, is)
		}
	}
	return paginate(ordered(list,
		func(v any) string { return idKey(v.(*store.Issue).ID) },
		func(key string) any {
			if is := q.st.IssueByID(keyID(key)); is != nil && is.Repo == r {
				return is
			}
			return nil
		},
		compare(by, asc)), a)
}

// pulls is Repository.pullRequests.
func (q *query) pulls(r *store.Repo, a args) (any, error) {
	by, asc := order(a)
	states := a.strings("states")
	head, base := a.str("headRefName"), a.str("baseRefName")
	labelled := len(a.list("labels")) > 0 // pull requests carry no labels, so a label filter matches none
	list := make([]any, 0, len(r.Pulls))
	for _, p := range r.Pulls {
		state := pullState(p)
		match := len(states) == 0
		for _, s := range states {
			match = match || s == state
		}
		if match && !labelled && (head == "" || p.HeadRef == head) && (base == "" || p.BaseRef == base) {
			list = append(list, p)
		}
	}
	rank := compare(by, asc)
	if by != store.SortCreated || !asc {
		sort.SliceStable(list, func(i, j int) bool { return rank(list[i], list[j]) < 0 })
	}
	return paginate(ordered(list,
		func(v any) string { return idKey(v.(*store.Pull).ID) },
		func(key string) any {
			if p := q.st.PullByID(keyID(key)); p != nil && p.Repo == r {
				return p
			}
			return nil
		},
		rank), a)
}

func pullState(p *store.Pull) string {
	switch {
	case p.Merged:
		return "MERGED"
	case p.Closed:
		return "CLOSED"
	}
	return "OPEN"
}

func comments(list []*store.Comment, a args) (any, error) {
	return paginate(identified(items(list), func(v any) string { return idKey(v.(*store.Comment).ID) }), a)
}

func labels(list []*store.Label, a args) (any, error) {
	return paginate(identified(items(list), func(v any) string { return idKey(v.(*store.Label).ID) }), a)
}

// ---- registration ----

// resolvers registers the resolver of every field of schema.graphql.
func resolvers(s *Schema) {
	call(s, "Query", callers[root]{
		"viewer": func(*query, root, args) (any, error) { return store.Viewer{}, nil },
		"repository": func(q *query, _ root, a args) (any, error) {
			// An unknown repository is null without an error, as is one of another owner.
			if r := q.st.Lookup([]byte(a.str("owner")), []byte(a.str("name"))); r != nil {
				return r, nil
			}
			return nil, nil
		},
		"node": func(q *query, _ root, a args) (any, error) {
			if n := q.node(a.str("id")); n != nil {
				return n, nil
			}
			return nil, notFound(a.str("id"))
		},
		"nodes": func(q *query, _ root, a args) (any, error) {
			ids := a.strings("ids")
			out := make([]any, len(ids))
			for i, id := range ids {
				if out[i] = q.node(id); out[i] == nil {
					q.failAt(i, notFound(id))
				}
			}
			return out, nil
		},
		"user": func(_ *query, _ root, a args) (any, error) {
			if strings.EqualFold(a.str("login"), store.ViewerLogin) {
				return store.Viewer{}, nil
			}
			return nil, &Error{Type: "NOT_FOUND", Message: "Could not resolve to a User with the login of '" + a.str("login") + "'."}
		},
		"repositoryOwner": func(_ *query, _ root, a args) (any, error) {
			if strings.EqualFold(a.str("login"), store.ViewerLogin) {
				return store.Viewer{}, nil
			}
			return nil, nil
		},
		"search": func(q *query, _ root, a args) (any, error) { return q.search(a) },
	})

	get(s, "User", getters[store.Viewer]{
		"id":         func(*query, store.Viewer) any { return store.ViewerNodeID },
		"databaseId": func(*query, store.Viewer) any { return store.ViewerID },
		"login":      func(*query, store.Viewer) any { return store.ViewerLogin },
		"name":       func(*query, store.Viewer) any { return store.ViewerName },
		"email":      func(*query, store.Viewer) any { return store.ViewerEmail },
		"url":        func(q *query, _ store.Viewer) any { return q.origin() + "/" + store.ViewerLogin },
	})
	call(s, "User", callers[store.Viewer]{
		"avatarUrl": func(q *query, _ store.Viewer, a args) (any, error) {
			url := q.origin() + "/avatars/u/" + strconv.Itoa(store.ViewerID) + "?v=4"
			if size, ok := a.int("size"); ok {
				url += "&s=" + strconv.Itoa(size)
			}
			return url, nil
		},
	})

	get(s, "Repository", getters[*store.Repo]{
		"id":            func(_ *query, r *store.Repo) any { return r.NodeID },
		"databaseId":    func(_ *query, r *store.Repo) any { return r.ID },
		"name":          func(_ *query, r *store.Repo) any { return r.Name },
		"nameWithOwner": func(_ *query, r *store.Repo) any { return r.FullName() },
		"owner":         func(*query, *store.Repo) any { return store.Viewer{} },
		"defaultBranchRef": func(_ *query, r *store.Repo) any {
			if r.Head() == nil {
				return nil // the default branch has no commit yet
			}
			return &ref{r, r.HeadRef()}
		},
		"viewerPermission": func(*query, *store.Repo) any { return "ADMIN" },
		"hasIssuesEnabled": func(_ *query, r *store.Repo) any { return r.HasIssues },
		"description": func(_ *query, r *store.Repo) any {
			if !r.HasDescription {
				return nil
			}
			return r.Description
		},
		"url":       func(q *query, r *store.Repo) any { return q.repoURL(r) },
		"isPrivate": func(_ *query, r *store.Repo) any { return r.Private },
		"visibility": func(_ *query, r *store.Repo) any {
			if r.Private {
				return "PRIVATE"
			}
			return "PUBLIC"
		},
		"isEmpty":    func(_ *query, r *store.Repo) any { return r.IsEmpty() },
		"isFork":     func(*query, *store.Repo) any { return false },
		"isArchived": func(_ *query, r *store.Repo) any { return r.Archived },
		"createdAt":  func(_ *query, r *store.Repo) any { return stamp(r.CreatedAt) },
		"updatedAt":  func(_ *query, r *store.Repo) any { return stamp(r.UpdatedAt) },
		"pushedAt":   func(_ *query, r *store.Repo) any { return stamp(r.PushedAt) },
	})
	call(s, "Repository", callers[*store.Repo]{
		"issue": func(_ *query, r *store.Repo, a args) (any, error) {
			n, _ := a.int("number")
			if is := r.Issue(int64(n)); is != nil {
				return is, nil
			}
			return nil, nil
		},
		"issues": func(q *query, r *store.Repo, a args) (any, error) { return q.issues(r, a) },
		"pullRequest": func(_ *query, r *store.Repo, a args) (any, error) {
			n, _ := a.int("number")
			if p := r.Pull(int64(n)); p != nil {
				return p, nil
			}
			return nil, nil
		},
		"pullRequests": func(q *query, r *store.Repo, a args) (any, error) { return q.pulls(r, a) },
		"ref": func(_ *query, r *store.Repo, a args) (any, error) {
			name := a.str("qualifiedName")
			for _, full := range [...]string{name, "refs/heads/" + name, "refs/tags/" + name, "refs/" + name} {
				if _, ok := r.Ref(full); ok && strings.HasPrefix(full, "refs/") {
					return &ref{r, full}, nil
				}
			}
			return nil, nil
		},
		"label": func(_ *query, r *store.Repo, a args) (any, error) {
			if l := r.Label(a.str("name")); l != nil {
				return l, nil
			}
			return nil, nil
		},
		"labels": func(_ *query, r *store.Repo, a args) (any, error) { return labels(r.Labels, a) },
	})

	get(s, "Ref", getters[*ref]{
		"id": func(_ *query, f *ref) any { return gitNodeID("REF", "Ref", f.repo, f.name) },
		"name": func(_ *query, f *ref) any {
			_, short := splitRef(f.name)
			return short
		},
		"prefix": func(_ *query, f *ref) any {
			prefix, _ := splitRef(f.name)
			return prefix
		},
		"target": func(_ *query, f *ref) any {
			id, _ := f.repo.Ref(f.name)
			if o := f.repo.Objects.Peel(id); o != nil && o.Commit != nil {
				return &commit{f.repo, o}
			}
			return nil
		},
	})
	get(s, "Commit", getters[*commit]{
		"id":  func(_ *query, c *commit) any { return gitNodeID("C", "Commit", c.repo, c.obj.ID.String()) },
		"oid": func(_ *query, c *commit) any { return c.obj.ID.String() },
		"message": func(_ *query, c *commit) any {
			parsed, _ := gitobj.ParseCommit(c.obj.Data)
			return parsed.Message
		},
	})

	get(s, "Issue", getters[*store.Issue]{
		"id":         func(_ *query, is *store.Issue) any { return store.NodeID(store.KindIssue, is.ID) },
		"number":     func(_ *query, is *store.Issue) any { return is.Number },
		"title":      func(_ *query, is *store.Issue) any { return is.Title },
		"body":       func(_ *query, is *store.Issue) any { return is.Body },
		"databaseId": func(_ *query, is *store.Issue) any { return is.ID },
		"closed":     func(_ *query, is *store.Issue) any { return is.Closed },
		"closedAt":   func(_ *query, is *store.Issue) any { return stamp(is.ClosedAt) },
		"createdAt":  func(_ *query, is *store.Issue) any { return stamp(is.CreatedAt) },
		"updatedAt":  func(_ *query, is *store.Issue) any { return stamp(is.UpdatedAt) },
		"author":     func(*query, *store.Issue) any { return store.Viewer{} },
		"repository": func(_ *query, is *store.Issue) any { return is.Repo },
		"state": func(_ *query, is *store.Issue) any {
			if is.Closed {
				return "CLOSED"
			}
			return "OPEN"
		},
		"stateReason": func(_ *query, is *store.Issue) any { return text(strings.ToUpper(is.StateReason)) },
		"url": func(q *query, is *store.Issue) any {
			return q.repoURL(is.Repo) + "/issues/" + strconv.FormatInt(is.Number, 10)
		},
	})
	call(s, "Issue", callers[*store.Issue]{
		"comments": func(_ *query, is *store.Issue, a args) (any, error) { return comments(is.Comments, a) },
		"labels":   func(_ *query, is *store.Issue, a args) (any, error) { return labels(is.Labels, a) },
	})

	get(s, "PullRequest", getters[*store.Pull]{
		"id":          func(_ *query, p *store.Pull) any { return store.NodeID(store.KindPull, p.ID) },
		"number":      func(_ *query, p *store.Pull) any { return p.Number },
		"state":       func(_ *query, p *store.Pull) any { return pullState(p) },
		"merged":      func(_ *query, p *store.Pull) any { return p.Merged },
		"headRefName": func(_ *query, p *store.Pull) any { return p.HeadRef },
		"baseRefName": func(_ *query, p *store.Pull) any { return p.BaseRef },
		"mergeable": func(_ *query, p *store.Pull) any {
			switch p.Repo.PullInfo(p).Mergeable {
			case store.MergeClean:
				return "MERGEABLE"
			case store.MergeConflicting:
				return "CONFLICTING"
			}
			return "UNKNOWN"
		},
		"title":      func(_ *query, p *store.Pull) any { return p.Title },
		"body":       func(_ *query, p *store.Pull) any { return p.Body },
		"isDraft":    func(_ *query, p *store.Pull) any { return p.Draft },
		"closed":     func(_ *query, p *store.Pull) any { return p.Closed },
		"mergedAt":   func(_ *query, p *store.Pull) any { return stamp(p.MergedAt) },
		"createdAt":  func(_ *query, p *store.Pull) any { return stamp(p.CreatedAt) },
		"updatedAt":  func(_ *query, p *store.Pull) any { return stamp(p.UpdatedAt) },
		"closedAt":   func(_ *query, p *store.Pull) any { return stamp(p.ClosedAt) },
		"author":     func(*query, *store.Pull) any { return store.Viewer{} },
		"repository": func(_ *query, p *store.Pull) any { return p.Repo },
		"headRefOid": func(_ *query, p *store.Pull) any { return p.Repo.PullInfo(p).Head.String() },
		"baseRefOid": func(_ *query, p *store.Pull) any { return p.Repo.PullInfo(p).Base.String() },
		"headRef":    func(_ *query, p *store.Pull) any { return branch(p.Repo, p.HeadRef) },
		"baseRef":    func(_ *query, p *store.Pull) any { return branch(p.Repo, p.BaseRef) },
		"additions":  func(_ *query, p *store.Pull) any { return p.Repo.PullInfo(p).Additions },
		"deletions":  func(_ *query, p *store.Pull) any { return p.Repo.PullInfo(p).Deletions },
		"changedFiles": func(_ *query, p *store.Pull) any {
			return len(p.Repo.PullInfo(p).Files)
		},
		"databaseId": func(_ *query, p *store.Pull) any { return p.ID },
		"url": func(q *query, p *store.Pull) any {
			return q.repoURL(p.Repo) + "/pull/" + strconv.FormatInt(p.Number, 10)
		},
	})
	call(s, "PullRequest", callers[*store.Pull]{
		"files": func(_ *query, p *store.Pull, a args) (any, error) {
			files := p.Repo.PullInfo(p).Files
			list := make([]any, len(files))
			for i := range files {
				list[i] = &files[i]
			}
			return paginate(identified(list, func(v any) string { return v.(*store.FileChange).Path }), a)
		},
		"comments": func(_ *query, p *store.Pull, a args) (any, error) { return comments(p.Comments, a) },
	})
	get(s, "PullRequestChangedFile", getters[*store.FileChange]{
		"path":      func(_ *query, f *store.FileChange) any { return f.Path },
		"additions": func(_ *query, f *store.FileChange) any { return f.Additions },
		"deletions": func(_ *query, f *store.FileChange) any { return f.Deletions },
		"changeType": func(_ *query, f *store.FileChange) any {
			switch f.Status {
			case "added":
				return "ADDED"
			case "removed":
				return "DELETED"
			}
			return "MODIFIED"
		},
	})

	get(s, "IssueComment", getters[*store.Comment]{
		"id":         func(_ *query, c *store.Comment) any { return store.NodeID(store.KindComment, c.ID) },
		"databaseId": func(_ *query, c *store.Comment) any { return c.ID },
		"body":       func(_ *query, c *store.Comment) any { return c.Body },
		"author":     func(*query, *store.Comment) any { return store.Viewer{} },
		"createdAt":  func(_ *query, c *store.Comment) any { return stamp(c.CreatedAt) },
		"url": func(q *query, c *store.Comment) any { // the html_url REST gives the comment
			return q.repoURL(c.Repo) + "/issues/" + strconv.FormatInt(c.Number, 10) + "#issuecomment-" + strconv.FormatInt(c.ID, 10)
		},
	})
	get(s, "Label", getters[*store.Label]{
		"id":    func(_ *query, l *store.Label) any { return store.NodeID(store.KindLabel, l.ID) },
		"name":  func(_ *query, l *store.Label) any { return l.Name },
		"color": func(_ *query, l *store.Label) any { return l.Color },
		"description": func(_ *query, l *store.Label) any {
			if !l.HasDescription {
				return nil
			}
			return l.Description
		},
	})

	connections(s)
	mutations(s)
}

// splitRef splits a full ref name into GitHub's prefix and short name:
// "refs/heads/feature/x" is "refs/heads/" and "feature/x".
func splitRef(full string) (prefix, short string) {
	for _, p := range [...]string{"refs/heads/", "refs/tags/", "refs/"} {
		if strings.HasPrefix(full, p) {
			return p, full[len(p):]
		}
	}
	return "", full
}
