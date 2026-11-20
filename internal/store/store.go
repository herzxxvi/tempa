// Package store is the forge's in-memory state and its domain operations: repositories and their git
// data, labels, issues, comments and pull requests. REST, GraphQL, MCP and git smart HTTP are thin
// adapters over this one package, which is what makes an object created on one surface read back
// identically on the others.
//
// Locking: the store has a single mutex. An adapter holds it for the whole of a request's handling
// (the dispatcher in internal/app takes it before calling a surface) and no method here locks. The
// pointers the store returns (*Repo, *Issue, ...) are the live objects: read them while holding the
// lock, never modify them, and never keep them after the request.
//
// Determinism: nothing here reads the wall clock or iterates a map to produce output. Time is a
// logical clock that advances one second per mutation; ids are counters; every list is an ordered
// slice. The same request sequence therefore produces the same bytes, ids and commit SHAs.
package store

import (
	"strings"
	"sync"

	"forge/internal/gitobj"
)

// The one user of the forge: the login the bearer token stands for. Nothing on the wire names it,
// so it is fixed here; it owns every repository.
const (
	ViewerLogin = "adaiversen"
	ViewerID    = 1000
	ViewerName  = "ada iversen"
	ViewerEmail = "adaiversen@example.invalid"
)

// DefaultBranch is the default branch of every new repository, born or not.
const DefaultBranch = "main"

// Epoch is the logical clock's origin, 2026-01-01T00:00:00Z. The n-th mutation happens at Epoch+n.
const Epoch = 1767225600

// Store is the whole state of the forge.
type Store struct {
	mu    sync.Mutex
	clock int64

	repos  []*Repo          // creation order; the order of every repository listing
	byName map[string]*Repo // exact name
	byFold map[string]*Repo // lowercased name, for the case-insensitive fallback

	// Objects by numeric id (index id-1); a deleted object leaves nil behind, ids are never reused.
	repoIDs    []*Repo
	issueIDs   []*Issue
	pullIDs    []*Pull
	commentIDs []*Comment
	labelIDs   []*Label

	arena gitobj.Arena
	host  string // last host a response was rendered for, see InternHost
}

// New returns an empty store.
func New() *Store {
	return &Store{
		clock:  Epoch,
		byName: make(map[string]*Repo, 512),
		byFold: make(map[string]*Repo, 512),
	}
}

// Lock takes the store mutex. Hold it while calling anything else in this package and while reading
// anything the store returned.
func (s *Store) Lock() { s.mu.Lock() }

// Unlock releases the store mutex.
func (s *Store) Unlock() { s.mu.Unlock() }

// Now returns the current logical time without advancing it.
func (s *Store) Now() int64 { return s.clock }

// Tick advances the logical clock by one second and returns the new time. Every mutating operation
// calls it once; callers outside this package (the git push path) do the same.
func (s *Store) Tick() int64 {
	s.clock++
	return s.clock
}

// InternHost returns host as a string, reusing the previous string when it is the same host, so
// remembering which host a cached rendering belongs to costs no allocation per request.
func (s *Store) InternHost(host []byte) string {
	if s.host != string(host) {
		s.host = string(host)
	}
	return s.host
}

// IsViewer reports whether owner is the viewer's login, ignoring ASCII case.
func IsViewer(owner []byte) bool {
	if len(owner) != len(ViewerLogin) {
		return false
	}
	for i := 0; i < len(owner); i++ {
		ch := owner[i]
		if ch >= 'A' && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		if ch != ViewerLogin[i] {
			return false
		}
	}
	return true
}

// NewRepo is the input of CreateRepo.
type NewRepo struct {
	Name           string
	Description    string
	HasDescription bool // false renders "description": null
	Private        bool
	NoIssues       bool // true creates the repository with issues disabled
}

// validRepoName accepts what GitHub accepts: 1 to 100 of [A-Za-z0-9._-], not "." or "..", and not
// ending in ".git" (the git URL space).
func validRepoName(name string) bool {
	if len(name) == 0 || len(name) > 100 || name == "." || name == ".." || strings.HasSuffix(name, ".git") {
		return false
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '.', ch == '-', ch == '_':
		default:
			return false
		}
	}
	return true
}

// CreateRepo creates an empty repository owned by the viewer: no refs, no objects, HEAD pointing at
// the unborn default branch "main". A name that is taken (compared without case) is the 422
// "name already exists on this account" error and leaves the existing repository untouched.
func (s *Store) CreateRepo(in NewRepo) (*Repo, error) {
	if in.Name == "" {
		return nil, errRepoCreate("missing_field", "")
	}
	if !validRepoName(in.Name) {
		return nil, errRepoCreate("custom", "name can only contain ASCII letters, digits, and the characters ., -, and _")
	}
	fold := strings.ToLower(in.Name)
	if s.byFold[fold] != nil {
		return nil, errRepoExists()
	}
	now := s.Tick()
	r := &Repo{
		Name:           in.Name,
		Description:    in.Description,
		HasDescription: in.HasDescription,
		Private:        in.Private,
		HasIssues:      !in.NoIssues,
		DefaultBranch:  DefaultBranch,
		CreatedAt:      now,
		UpdatedAt:      now,
		PushedAt:       now,
		Objects:        gitobj.NewDB(&s.arena),
		store:          s,
	}
	s.repoIDs = append(s.repoIDs, r)
	r.ID = int64(len(s.repoIDs))
	r.NodeID = NodeID(KindRepo, r.ID)
	r.head = "refs/heads/" + r.DefaultBranch
	s.repos = append(s.repos, r)
	s.byName[r.Name] = r
	s.byFold[fold] = r
	return r, nil
}

// Repo returns the repository called name, or nil. The match is exact first, then case-insensitive.
func (s *Store) Repo(name string) *Repo {
	if r := s.byName[name]; r != nil {
		return r
	}
	return s.byFold[strings.ToLower(name)]
}

// RepoB is Repo for a name still sitting in a request buffer; it does not allocate.
func (s *Store) RepoB(name []byte) *Repo {
	if r := s.byName[string(name)]; r != nil {
		return r
	}
	var buf [100]byte
	if len(name) > len(buf) {
		return nil
	}
	lower := buf[:len(name)]
	for i, ch := range name {
		if ch >= 'A' && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		lower[i] = ch
	}
	return s.byFold[string(lower)]
}

// Lookup resolves the {owner}/{repo} pair of a URL: nil unless owner is the viewer and the
// repository exists. Adapters answer nil with a 404, never a 403.
func (s *Store) Lookup(owner, name []byte) *Repo {
	if !IsViewer(owner) {
		return nil
	}
	return s.RepoB(name)
}

// Repos returns every repository in creation order. The slice is the store's own: do not modify it.
func (s *Store) Repos() []*Repo { return s.repos }

// RepoPatch is the input of UpdateRepo; nil fields are left alone.
type RepoPatch struct {
	Name             *string
	Description      *string
	ClearDescription bool // "description": null
	Private          *bool
	HasIssues        *bool
	Archived         *bool
	DefaultBranch    *string
}

// UpdateRepo applies a patch. A rename must be a valid, unused name (422 otherwise) and keeps the
// repository's id, git data, issues and pull requests; a new default branch must exist.
func (s *Store) UpdateRepo(r *Repo, p RepoPatch) error {
	if p.Name != nil && *p.Name != r.Name {
		if !validRepoName(*p.Name) {
			return errValidation("Repository", "name", "custom", "", "name can only contain ASCII letters, digits, and the characters ., -, and _")
		}
		fold := strings.ToLower(*p.Name)
		if other := s.byFold[fold]; other != nil && other != r {
			return errValidation("Repository", "name", "custom", "", "name already exists on this account")
		}
	}
	if p.DefaultBranch != nil && *p.DefaultBranch != r.DefaultBranch {
		if _, ok := r.Branch(*p.DefaultBranch); !ok {
			return errValidation("Repository", "default_branch", "invalid", *p.DefaultBranch, "")
		}
	}
	r.UpdatedAt = s.Tick()
	if p.Name != nil && *p.Name != r.Name {
		delete(s.byName, r.Name)
		delete(s.byFold, strings.ToLower(r.Name))
		r.Name = *p.Name
		s.byName[r.Name] = r
		s.byFold[strings.ToLower(r.Name)] = r
		r.JSONGen++ // every cached rendering that embeds the name or a URL is stale
	}
	if p.ClearDescription {
		r.Description, r.HasDescription = "", false
	}
	if p.Description != nil {
		r.Description, r.HasDescription = *p.Description, true
	}
	if p.Private != nil {
		r.Private = *p.Private
	}
	if p.HasIssues != nil {
		r.HasIssues = *p.HasIssues
	}
	if p.Archived != nil {
		r.Archived = *p.Archived
	}
	if p.DefaultBranch != nil && *p.DefaultBranch != r.DefaultBranch {
		r.DefaultBranch = *p.DefaultBranch
		r.head = "refs/heads/" + r.DefaultBranch
		r.refsChanged() // HEAD moved: git advertisements are stale
	}
	return nil
}

// DeleteRepo removes a repository with everything in it: refs, objects, labels, issues, comments
// and pull requests. A repository created later under the same name starts empty, with new ids and
// numbers from 1.
func (s *Store) DeleteRepo(r *Repo) {
	if r.deleted {
		return
	}
	s.Tick()
	r.deleted = true
	delete(s.byName, r.Name)
	delete(s.byFold, strings.ToLower(r.Name))
	for i, x := range s.repos {
		if x == r {
			s.repos = append(s.repos[:i], s.repos[i+1:]...)
			break
		}
	}
	s.repoIDs[r.ID-1] = nil
	for _, is := range r.Issues {
		s.issueIDs[is.ID-1] = nil
		for _, c := range is.Comments {
			s.commentIDs[c.ID-1] = nil
		}
	}
	for _, p := range r.Pulls {
		s.pullIDs[p.ID-1] = nil
		for _, c := range p.Comments {
			s.commentIDs[c.ID-1] = nil
		}
	}
	for _, l := range r.Labels {
		s.labelIDs[l.ID-1] = nil
	}
	r.refs = nil
	r.Objects = gitobj.NewDB(&s.arena)
	r.refsChanged()
}

// RepoByID returns the repository with the numeric id, or nil.
func (s *Store) RepoByID(id int64) *Repo {
	if id < 1 || id > int64(len(s.repoIDs)) {
		return nil
	}
	return s.repoIDs[id-1]
}

// IssueByID returns the issue with the numeric id, or nil.
func (s *Store) IssueByID(id int64) *Issue {
	if id < 1 || id > int64(len(s.issueIDs)) {
		return nil
	}
	return s.issueIDs[id-1]
}

// PullByID returns the pull request with the numeric id, or nil.
func (s *Store) PullByID(id int64) *Pull {
	if id < 1 || id > int64(len(s.pullIDs)) {
		return nil
	}
	return s.pullIDs[id-1]
}

// CommentByID returns the comment with the numeric id, or nil.
func (s *Store) CommentByID(id int64) *Comment {
	if id < 1 || id > int64(len(s.commentIDs)) {
		return nil
	}
	return s.commentIDs[id-1]
}

// LabelByID returns the label with the numeric id, or nil.
func (s *Store) LabelByID(id int64) *Label {
	if id < 1 || id > int64(len(s.labelIDs)) {
		return nil
	}
	return s.labelIDs[id-1]
}
