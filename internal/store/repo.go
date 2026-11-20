package store

import (
	"strings"

	"forge/internal/gitobj"
)

// Ref is a named pointer into the object database, with its full name ("refs/heads/main").
type Ref struct {
	Name string
	ID   gitobj.OID
}

// GitCache holds whole pre-rendered git smart-HTTP responses for the current ref state. The git
// surface fills it lazily; every ref change empties it (see refsChanged). Entries are keyed by this
// repository object, never by name, so a deleted and recreated repository cannot see stale bytes.
type GitCache struct {
	AdvReceive  []byte            // receive-pack advertisement
	AdvUploadV0 []byte            // protocol v0 upload-pack advertisement
	Upload      map[string][]byte // upload-pack request body -> response
}

// numbered is one slot of the shared issue / pull request number sequence.
type numbered struct {
	issue *Issue
	pull  *Pull
}

// Repo is one repository. Exported fields are read-only for adapters.
type Repo struct {
	ID             int64
	NodeID         string // GraphQL global id, also REST node_id
	Name           string
	Description    string
	HasDescription bool
	Private        bool
	HasIssues      bool
	Archived       bool
	DefaultBranch  string
	CreatedAt      int64
	UpdatedAt      int64
	PushedAt       int64

	// Objects is the git object database. The git surface adds pushed objects to it directly.
	Objects *gitobj.DB
	// Gen counts ref changes; anything derived from refs can be cached against it.
	Gen uint64
	// Git is the git surface's response cache for the current Gen.
	Git GitCache

	Labels []*Label // creation order
	Issues []*Issue // creation order = ascending number; pull requests are not in here
	Pulls  []*Pull  // creation order = ascending number
	// OpenIssues counts open issues and open pull requests (REST open_issues_count).
	OpenIssues int
	// JSONGen changes when something embedded in this repository's cached renderings changes (a
	// rename). Cached JSON of its issues is valid only for the JSONGen it was rendered at.
	JSONGen uint64

	refs    []Ref // sorted by name
	head    string
	items   []numbered // index number-1
	store   *Store
	deleted bool
}

// FullName returns "adaiversen/<name>".
func (r *Repo) FullName() string { return ViewerLogin + "/" + r.Name }

// HeadRef returns the ref HEAD points at: "refs/heads/" + DefaultBranch, whether it exists or not.
func (r *Repo) HeadRef() string { return r.head }

// IsEmpty reports whether the repository has no refs at all (nothing was ever pushed or committed).
func (r *Repo) IsEmpty() bool { return len(r.refs) == 0 }

// Refs returns every ref sorted by name. The slice is the repository's own: do not modify it.
func (r *Repo) Refs() []Ref { return r.refs }

func (r *Repo) refIndex(name string) (int, bool) {
	lo, hi := 0, len(r.refs)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if r.refs[mid].Name < name {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < len(r.refs) && r.refs[lo].Name == name
}

// Ref returns the id a full ref name points at.
func (r *Repo) Ref(name string) (gitobj.OID, bool) {
	if i, ok := r.refIndex(name); ok {
		return r.refs[i].ID, true
	}
	return gitobj.Zero, false
}

// Branch returns the tip of a branch given its short name ("main", "feature/x").
func (r *Repo) Branch(name string) (gitobj.OID, bool) {
	for i := range r.refs {
		ref := &r.refs[i]
		if len(ref.Name) == len("refs/heads/")+len(name) && ref.Name[:11] == "refs/heads/" && ref.Name[11:] == name {
			return ref.ID, true
		}
	}
	return gitobj.Zero, false
}

// withPrefix returns the contiguous run of refs whose names start with prefix.
func (r *Repo) withPrefix(prefix string) []Ref {
	lo, _ := r.refIndex(prefix)
	hi := lo
	for hi < len(r.refs) && strings.HasPrefix(r.refs[hi].Name, prefix) {
		hi++
	}
	return r.refs[lo:hi:hi]
}

// Branches returns the refs under refs/heads/, sorted by name, with their full names. The slice is
// the repository's own: do not modify it.
func (r *Repo) Branches() []Ref { return r.withPrefix("refs/heads/") }

// Tags returns the refs under refs/tags/, sorted by name.
func (r *Repo) Tags() []Ref { return r.withPrefix("refs/tags/") }

// Head returns the commit HEAD resolves to, or nil while the default branch is unborn.
func (r *Repo) Head() *gitobj.Object {
	if id, ok := r.Ref(r.head); ok {
		return r.Objects.GetCommit(id)
	}
	return nil
}

// refsChanged must run after every change to refs or HEAD: it invalidates everything cached
// against the old ref state.
func (r *Repo) refsChanged() {
	r.Gen++
	r.Git.AdvReceive, r.Git.AdvUploadV0 = nil, nil
	clear(r.Git.Upload)
	r.PushedAt = r.store.clock
}

// SetRef creates the ref or moves it, with no checks at all: the caller (a domain operation here, or
// the git surface after its own fast-forward test) has decided the update is allowed.
func (r *Repo) SetRef(name string, id gitobj.OID) {
	i, ok := r.refIndex(name)
	if ok {
		if r.refs[i].ID == id {
			return
		}
		r.refs[i].ID = id
	} else {
		r.refs = append(r.refs, Ref{})
		copy(r.refs[i+1:], r.refs[i:])
		r.refs[i] = Ref{Name: name, ID: id}
	}
	r.refsChanged()
}

// RemoveRef deletes a ref and reports whether it existed.
func (r *Repo) RemoveRef(name string) bool {
	i, ok := r.refIndex(name)
	if !ok {
		return false
	}
	r.refs = append(r.refs[:i], r.refs[i+1:]...)
	r.refsChanged()
	return true
}

// ValidRefName reports whether name is an acceptable full ref name: "refs/<kind>/<name...>" with
// non-empty components that git would accept.
func ValidRefName(name string) bool {
	if !strings.HasPrefix(name, "refs/") || strings.Count(name, "/") < 2 || strings.HasSuffix(name, "/") ||
		strings.HasSuffix(name, ".") || strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part[0] == '.' || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for i := 0; i < len(name); i++ {
		switch ch := name[i]; {
		case ch <= ' ', ch == 0x7f, ch == '~', ch == '^', ch == ':', ch == '?', ch == '*', ch == '[', ch == '\\':
			return false
		}
	}
	return true
}

// CreateRef is POST /git/refs: create a ref at an existing object. Errors (all 422): "Reference
// name is invalid", "Object does not exist" (unknown sha, or a branch pointed at a non-commit),
// "Reference already exists".
func (r *Repo) CreateRef(name string, id gitobj.OID) error {
	if !ValidRefName(name) {
		return errStatus(422, "Reference name is invalid")
	}
	o := r.Objects.Get(id)
	if o == nil || (strings.HasPrefix(name, "refs/heads/") && o.Commit == nil) {
		return errStatus(422, "Object does not exist")
	}
	if _, ok := r.Ref(name); ok {
		return errStatus(422, "Reference already exists")
	}
	r.store.Tick()
	r.SetRef(name, id)
	return nil
}

// UpdateRef is PATCH /git/refs/{ref}: move an existing ref. Without force a branch only moves
// forward. Errors (all 422): "Reference does not exist", "Object does not exist", "Update is not a
// fast forward".
func (r *Repo) UpdateRef(name string, id gitobj.OID, force bool) error {
	old, ok := r.Ref(name)
	if !ok {
		return errStatus(422, "Reference does not exist")
	}
	o := r.Objects.Get(id)
	if o == nil || (strings.HasPrefix(name, "refs/heads/") && o.Commit == nil) {
		return errStatus(422, "Object does not exist")
	}
	if !force && !r.Objects.IsAncestor(old, id) {
		return errStatus(422, "Update is not a fast forward")
	}
	r.store.Tick()
	r.SetRef(name, id)
	return nil
}

// DeleteRef is DELETE /git/refs/{ref}. A missing ref is the 422 "Reference does not exist".
func (r *Repo) DeleteRef(name string) error {
	if _, ok := r.Ref(name); !ok {
		return errStatus(422, "Reference does not exist")
	}
	r.store.Tick()
	r.RemoveRef(name)
	return nil
}

// Resolve turns a revision into a commit. rev may be empty (the default branch), a branch name, a
// tag name, a full ref name ("refs/heads/x", also "heads/x" and "tags/x") or a 40-hex commit id.
// Tags are peeled. It returns nil when rev names no commit in this repository.
func (r *Repo) Resolve(rev string) *gitobj.Object {
	if rev == "" {
		return r.Head()
	}
	if id, ok := r.Branch(rev); ok {
		return r.Objects.GetCommit(id)
	}
	for _, name := range [...]string{rev, "refs/tags/" + rev, "refs/" + rev} {
		if !strings.HasPrefix(name, "refs/") {
			continue
		}
		if id, ok := r.Ref(name); ok {
			if o := r.Objects.Peel(id); o != nil && o.Commit != nil {
				return o
			}
			return nil
		}
	}
	if id, ok := gitobj.ParseOID(rev); ok {
		if o := r.Objects.Peel(id); o != nil && o.Commit != nil {
			return o
		}
	}
	return nil
}

// Log lists the history of rev (see Resolve), newest first, skipping offset commits and appending
// at most limit commits to dst (limit <= 0: no limit). more reports whether further commits exist
// beyond the returned page. Errors: 409 "Git Repository is empty." for a repository without refs,
// 404 "No commit found for SHA: <rev>" for an unknown revision.
func (r *Repo) Log(rev string, offset, limit int, dst []*gitobj.Object) (commits []*gitobj.Object, more bool, err error) {
	if r.IsEmpty() {
		return dst, false, errStatus(409, MsgEmptyGit)
	}
	tip := r.Resolve(rev)
	if tip == nil {
		if rev == "" {
			rev = r.DefaultBranch
		}
		return dst, false, errStatus(404, "No commit found for SHA: "+rev)
	}
	skipped := 0
	base := len(dst)
	r.Objects.Walk(tip.ID, func(o *gitobj.Object) bool {
		if skipped < offset {
			skipped++
			return true
		}
		if limit > 0 && len(dst)-base == limit {
			more = true
			return false
		}
		dst = append(dst, o)
		return true
	})
	return dst, more, nil
}
