package store

import (
	"sort"
	"strconv"
	"strings"

	"forge/internal/gitobj"
)

// Pull is a pull request between two branches of one repository.
type Pull struct {
	ID      int64
	Number  int64
	Title   string
	Body    string // "" when created without one; never rendered as null
	HeadRef string // short branch names
	BaseRef string
	Draft   bool
	Closed  bool // true for closed and for merged pull requests
	Merged  bool

	// Head and Base are the branch tips as last seen while the pull request was open. Once it is
	// closed or merged they are frozen: Base is the base tip the merge was made onto.
	Head, Base  gitobj.OID
	MergeCommit gitobj.OID // the commit a merge created; Zero until merged

	Comments  []*Comment
	CreatedAt int64
	UpdatedAt int64
	ClosedAt  int64
	MergedAt  int64
	Repo      *Repo

	info *PullInfo
}

// Mergeable is the answer to "would this pull request merge cleanly right now".
type Mergeable uint8

const (
	MergeUnknown     Mergeable = iota // closed or merged, or a branch is gone
	MergeClean                        // REST mergeable true / "clean", GraphQL MERGEABLE
	MergeConflicting                  // REST mergeable false / "dirty", GraphQL CONFLICTING
)

// FileChange is one changed file of a pull request.
type FileChange struct {
	Path      string
	Status    string // "added", "removed" or "modified"
	Additions int
	Deletions int
	Blob      gitobj.OID // the file's blob on the head side; the old blob for a removed file
}

// PullInfo is everything about a pull request that depends on the git state: where its branches
// are, what it changes and whether it merges. It is computed on demand from the current refs and
// cached until a tip moves.
type PullInfo struct {
	Head, Base gitobj.OID // current tips (frozen values once closed)
	MergeBase  gitobj.OID // Zero when the branches share no history
	Mergeable  Mergeable
	Files      []FileChange // merge base -> head, sorted by path
	Commits    int          // commits on head that are not on base
	Additions  int
	Deletions  int

	closed bool
}

// NewPull is the input of CreatePull.
type NewPull struct {
	Title string
	Body  string
	Head  string // "branch" or "owner:branch"
	Base  string
	Draft bool
}

// CreatePull is POST /pulls. Head may carry an "owner:" prefix, which is dropped. Both branches
// must exist; an identical head and base, or an open pull request for the same pair, is refused.
// "No commits between the branches" is not an error. The new pull request takes the next number of
// the sequence shared with issues.
//
// Errors (422 Validation Failed, resource PullRequest): missing_field for title, head or base;
// invalid head or base for a branch that does not exist; custom "A pull request already exists for
// adaiversen:<head>." for a duplicate.
func (r *Repo) CreatePull(in NewPull) (*Pull, error) {
	if in.Title == "" {
		return nil, errMissing("PullRequest", "title")
	}
	if in.Head == "" {
		return nil, errMissing("PullRequest", "head")
	}
	if in.Base == "" {
		return nil, errMissing("PullRequest", "base")
	}
	head := in.Head
	if i := strings.LastIndexByte(head, ':'); i >= 0 {
		head = head[i+1:]
	}
	headID, ok := r.Branch(head)
	if !ok || head == in.Base {
		return nil, errValidation("PullRequest", "head", "invalid", "", "")
	}
	baseID, ok := r.Branch(in.Base)
	if !ok {
		return nil, errValidation("PullRequest", "base", "invalid", "", "")
	}
	for _, p := range r.Pulls {
		if !p.Closed && p.HeadRef == head && p.BaseRef == in.Base {
			return nil, errValidation("PullRequest", "", "custom", "", "A pull request already exists for "+ViewerLogin+":"+head+".")
		}
	}
	s := r.store
	now := s.Tick()
	p := &Pull{
		Title: in.Title, Body: in.Body, HeadRef: head, BaseRef: in.Base, Draft: in.Draft,
		Head: headID, Base: baseID, CreatedAt: now, UpdatedAt: now, Repo: r,
	}
	s.pullIDs = append(s.pullIDs, p)
	p.ID = int64(len(s.pullIDs))
	p.Number = r.nextNumber()
	r.items[p.Number-1].pull = p
	r.Pulls = append(r.Pulls, p)
	r.OpenIssues++
	return p, nil
}

// Pull returns the pull request with the given number, or nil when there is none or the number
// belongs to an issue.
func (r *Repo) Pull(number int64) *Pull {
	if number < 1 || number > int64(len(r.items)) {
		return nil
	}
	return r.items[number-1].pull
}

// PullQuery selects and orders pull requests.
type PullQuery struct {
	State  State  // StateClosed includes merged pull requests
	Head   string // "branch" or "owner:branch"; empty = any
	Base   string // empty = any
	Sort   Sort   // SortCreated or SortUpdated
	Asc    bool   // false = newest first, the REST default
	Offset int
	Limit  int // <= 0 = no limit
}

// ListPulls appends the selected page of pull requests to dst and returns it with the number that
// matched before paging.
func (r *Repo) ListPulls(q PullQuery, dst []*Pull) (page []*Pull, total int) {
	head := q.Head
	if i := strings.LastIndexByte(head, ':'); i >= 0 {
		head = head[i+1:]
	}
	base := len(dst)
	for _, p := range r.Pulls {
		if (q.State == StateOpen && p.Closed) || (q.State == StateClosed && !p.Closed) ||
			(head != "" && p.HeadRef != head) || (q.Base != "" && p.BaseRef != q.Base) {
			continue
		}
		dst = append(dst, p)
	}
	all := dst[base:]
	total = len(all)
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if q.Sort == SortUpdated && a.UpdatedAt != b.UpdatedAt {
			return (a.UpdatedAt < b.UpdatedAt) == q.Asc
		}
		return (a.Number < b.Number) == q.Asc
	})
	lo := min(q.Offset, total)
	hi := total
	if q.Limit > 0 && lo+q.Limit < hi {
		hi = lo + q.Limit
	}
	return append(dst[:base], all[lo:hi]...), total
}

// PullPatch is the input of UpdatePull; nil fields are left alone.
type PullPatch struct {
	Title *string
	Body  *string
	State *string // "open" or "closed"
}

// UpdatePull is PATCH /pulls/{n}: edit the title or body, close an open pull request, or reopen a
// closed one that was not merged. Errors (422 Validation Failed): empty title, bad state, reopening
// a merged pull request.
func (r *Repo) UpdatePull(p *Pull, patch PullPatch) error {
	if patch.Title != nil && *patch.Title == "" {
		return errMissing("PullRequest", "title")
	}
	if patch.State != nil {
		if *patch.State != "open" && *patch.State != "closed" {
			return errValidation("PullRequest", "state", "invalid", *patch.State, "")
		}
		if *patch.State == "open" && p.Merged {
			return errValidation("PullRequest", "state", "invalid", "open", "")
		}
	}
	info := r.PullInfo(p) // settle Head and Base before a state change freezes them
	now := r.store.Tick()
	if patch.Title != nil {
		p.Title = *patch.Title
	}
	if patch.Body != nil {
		p.Body = *patch.Body
	}
	if patch.State != nil {
		switch closing := *patch.State == "closed"; {
		case closing && !p.Closed:
			p.Closed, p.ClosedAt = true, now
			p.Head, p.Base = info.Head, info.Base
			r.OpenIssues--
		case !closing && p.Closed:
			p.Closed, p.ClosedAt = false, 0
			r.OpenIssues++
		}
		p.info = nil
	}
	p.UpdatedAt = now
	return nil
}

// PullInfo returns the git-dependent view of a pull request. For an open pull request the tips are
// the branches' current tips (a branch that was deleted keeps its last seen tip); for a closed or
// merged one they are the frozen values, so the diff stays the one it had. The result is cached and
// must not be modified.
func (r *Repo) PullInfo(p *Pull) *PullInfo {
	if !p.Closed {
		if id, ok := r.Branch(p.HeadRef); ok {
			p.Head = id
		}
		if id, ok := r.Branch(p.BaseRef); ok {
			p.Base = id
		}
	}
	if in := p.info; in != nil && in.Head == p.Head && in.Base == p.Base && in.closed == p.Closed {
		return in
	}
	db := r.Objects
	in := &PullInfo{Head: p.Head, Base: p.Base, closed: p.Closed}
	headTree, baseTree, mbTree := treeOf(db, p.Head), treeOf(db, p.Base), gitobj.Zero
	if mb, ok := db.MergeBase(p.Base, p.Head); ok {
		in.MergeBase = mb
		mbTree = treeOf(db, mb)
	}
	changes := db.DiffTrees(mbTree, headTree)
	in.Files = make([]FileChange, len(changes))
	for i := range changes {
		c := &changes[i]
		f := &in.Files[i]
		f.Path, f.Blob, f.Status = c.Path, c.New, "modified"
		switch {
		case c.Added():
			f.Status = "added"
		case c.Removed():
			f.Status, f.Blob = "removed", c.Old
		}
		f.Additions, f.Deletions = db.ChangeStats(c)
		in.Additions += f.Additions
		in.Deletions += f.Deletions
	}
	in.Commits = db.CountAhead(p.Head, p.Base)
	if !p.Closed && db.GetCommit(p.Head) != nil && db.GetCommit(p.Base) != nil {
		in.Mergeable = MergeConflicting
		if _, ok := db.Merge3(mbTree, baseTree, headTree, false); ok {
			in.Mergeable = MergeClean
		}
	}
	p.info = in
	return in
}

func treeOf(db *gitobj.DB, commit gitobj.OID) gitobj.OID {
	if o := db.GetCommit(commit); o != nil {
		return o.Commit.Tree
	}
	return gitobj.Zero
}

// MergeOptions is the input of MergePull.
type MergeOptions struct {
	Method        string // "merge" (default), "squash" or "rebase" (performed as a squash)
	CommitTitle   string // overrides the default first line of the commit message
	CommitMessage string // commit message body
	SHA           string // when set, the head tip the caller expects
}

// MergePull is PUT /pulls/{n}/merge. Mergeability is decided now, against the current tip of the
// base branch, with the three-way tree merge of gitobj.Merge3 (path-level, no line merging).
//
//   - squash, rebase: one new commit whose only parent is the base tip and whose tree is the merge
//     result; message "<title> (#<n>)" (or CommitTitle), then CommitMessage or, when the pull request
//     has several commits, one "* <subject>" line per commit, oldest first.
//   - merge: a commit with parents [base tip, head tip] and message
//     "Merge pull request #<n> from adaiversen/<head>\n\n<title>".
//
// The base branch moves to the new commit; the pull request becomes closed and merged with
// MergeCommit set and Head/Base frozen; the head branch stays. Author and committer are the viewer
// at the logical time of the merge.
//
// Errors: 405 "Pull Request is not mergeable" when it is not open, a branch is gone, or the merge
// conflicts (nothing is written); 409 when SHA is set and is not the head tip; 422 for an unknown
// merge method.
func (r *Repo) MergePull(p *Pull, opt MergeOptions) (*gitobj.Object, error) {
	method := opt.Method
	switch method {
	case "":
		method = "merge"
	case "merge", "squash", "rebase":
	default:
		return nil, errValidation("PullRequest", "merge_method", "invalid", opt.Method, "")
	}
	if p.Closed {
		return nil, errStatus(405, MsgNotMergeable)
	}
	db := r.Objects
	headID, okHead := r.Branch(p.HeadRef)
	baseID, okBase := r.Branch(p.BaseRef)
	head, base := db.GetCommit(headID), db.GetCommit(baseID)
	if !okHead || !okBase || head == nil || base == nil {
		return nil, errStatus(405, MsgNotMergeable)
	}
	if opt.SHA != "" && !strings.EqualFold(opt.SHA, headID.String()) {
		return nil, errStatus(409, MsgHeadModified)
	}
	mbTree := gitobj.Zero
	if mb, ok := db.MergeBase(baseID, headID); ok {
		mbTree = treeOf(db, mb)
	}
	if _, ok := db.Merge3(mbTree, base.Commit.Tree, head.Commit.Tree, false); !ok {
		return nil, errStatus(405, MsgNotMergeable)
	}
	number := strconv.FormatInt(p.Number, 10)
	var message string
	parents := []gitobj.OID{baseID}
	if method == "merge" {
		parents = append(parents, headID)
		message = "Merge pull request #" + number + " from " + ViewerLogin + "/" + p.HeadRef + "\n\n" + p.Title
		if opt.CommitTitle != "" {
			message = opt.CommitTitle
		}
		if opt.CommitMessage != "" {
			message = Subject(message) + "\n\n" + opt.CommitMessage
		}
	} else {
		message = p.Title + " (#" + number + ")"
		if opt.CommitTitle != "" {
			message = opt.CommitTitle
		}
		if opt.CommitMessage != "" {
			message += "\n\n" + opt.CommitMessage
		} else if subjects := r.aheadSubjects(headID, baseID); len(subjects) > 1 {
			message += "\n"
			for i := len(subjects) - 1; i >= 0; i-- {
				message += "\n* " + subjects[i]
			}
		}
	}
	now := r.store.Tick()
	tree, _ := db.Merge3(mbTree, base.Commit.Tree, head.Commit.Tree, true)
	commit := r.commit(tree, parents, message, nil, nil, now)
	r.SetRef("refs/heads/"+p.BaseRef, commit.ID)
	p.Closed, p.Merged = true, true
	p.ClosedAt, p.MergedAt, p.UpdatedAt = now, now, now
	p.Head, p.Base, p.MergeCommit = headID, baseID, commit.ID
	p.info = nil
	r.OpenIssues--
	return commit, nil
}

// Subject returns the first line of a commit message.
func Subject(message string) string { return gitobj.Subject(message) }

// aheadSubjects returns the subjects of the commits on head that are not on base, newest first.
func (r *Repo) aheadSubjects(head, base gitobj.OID) []string {
	n := r.Objects.CountAhead(head, base)
	subjects := make([]string, 0, n)
	r.Objects.Walk(head, func(o *gitobj.Object) bool {
		if len(subjects) == n {
			return false
		}
		if c, err := gitobj.ParseCommit(o.Data); err == nil {
			subjects = append(subjects, gitobj.Subject(c.Message))
		}
		return true
	})
	return subjects
}
