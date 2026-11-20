package store

import (
	"sort"
	"strings"

	"forge/internal/gitobj"
)

// Content is the answer of Contents: a file with its bytes, or a directory with its entries.
type Content struct {
	Commit  *gitobj.Object // the commit the path was read from
	IsDir   bool
	Mode    uint32         // file: the tree entry's mode (ModeFile, ModeExec, ModeSymlink, ModeGitlink)
	ID      gitobj.OID     // file: blob id; directory: tree id
	Data    []byte         // file: the blob's bytes, read-only; nil for a gitlink
	Entries []gitobj.Entry // directory: entries sorted by name
}

// Contents reads path at revision rev (see Resolve; empty = default branch). The empty path is the
// root directory. Errors, all 404: "This repository is empty." (no refs), "No commit found for the
// ref <rev>", and "Not Found" for a path that does not exist.
func (r *Repo) Contents(path, rev string) (Content, error) {
	var c Content
	if r.IsEmpty() {
		return c, errStatus(404, MsgEmptyRepo)
	}
	commit := r.Resolve(rev)
	if commit == nil {
		if rev == "" {
			rev = r.DefaultBranch
		}
		return c, errStatus(404, "No commit found for the ref "+rev)
	}
	path = strings.Trim(path, "/")
	e, ok := r.Objects.Find(commit.Commit.Tree, path)
	if !ok {
		return c, ErrNotFound
	}
	c.Commit, c.Mode, c.ID = commit, e.Mode, e.ID
	if e.IsDir() {
		entries, err := r.Objects.Tree(e.ID)
		if err != nil {
			return c, ErrNotFound
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		c.IsDir, c.Entries = true, entries
		return c, nil
	}
	if e.Mode != gitobj.ModeGitlink {
		blob := r.Objects.Get(e.ID)
		if blob == nil || blob.Type != gitobj.TypeBlob {
			return c, ErrNotFound
		}
		c.Data = blob.Data
	}
	return c, nil
}

// Ident names the author or committer of a commit made through the API.
type Ident struct {
	Name  string
	Email string
}

// FileWrite is the input of PutFile and DeleteFile.
type FileWrite struct {
	Path      string // slash-separated, already percent-decoded
	Message   string // commit message, stored byte for byte
	Data      []byte // PutFile: the new file content
	SHA       string // blob id the caller believes is current; required to update or delete
	Branch    string // empty = the default branch
	Author    *Ident // nil = the viewer
	Committer *Ident // nil = Author, or the viewer
}

// FileResult is what a contents write produced.
type FileResult struct {
	Created bool   // PutFile: the path did not exist before (REST 201 rather than 200)
	Branch  string // branch the commit landed on
	Path    string
	Mode    uint32
	Blob    gitobj.OID     // PutFile: id of the stored blob
	Size    int            // PutFile: length of the content
	Commit  *gitobj.Object // the new commit
}

// commit builds a commit on top of parent (nil for a root commit), stamped with the logical clock.
func (r *Repo) commit(tree gitobj.OID, parents []gitobj.OID, message string, author, committer *Ident, now int64) *gitobj.Object {
	c := gitobj.Commit{Tree: tree, Parents: parents, Message: message}
	c.Author = gitobj.Signature{Name: ViewerName, Email: ViewerEmail, When: now}
	if author != nil && author.Name != "" && author.Email != "" {
		c.Author.Name, c.Author.Email = author.Name, author.Email
	}
	c.Committer = c.Author
	if committer != nil && committer.Name != "" && committer.Email != "" {
		c.Committer.Name, c.Committer.Email = committer.Name, committer.Email
	}
	return r.Objects.Put(gitobj.TypeCommit, gitobj.AppendCommit(nil, &c))
}

// writeTarget finds where a contents write goes: the branch name and its current tip (nil when the
// write will create the branch from nothing).
//   - no refs at all: the write creates the branch with a root commit;
//   - the branch exists: its tip;
//   - the branch is missing in a repository with history: it is created from the default branch's
//     tip (lenient: GitHub answers 404 here, but clients rely on a contents write creating a branch).
func (r *Repo) writeTarget(branch string) (name string, tip *gitobj.Object) {
	if branch == "" {
		branch = r.DefaultBranch
	}
	if id, ok := r.Branch(branch); ok {
		return branch, r.Objects.GetCommit(id)
	}
	return branch, r.Head()
}

// PutFile is PUT /contents/{path}: create or update one file with one commit on a branch.
//
// Errors: 422 for a missing message, an invalid path, a path that is a directory or runs through a
// file, and for an existing file when SHA is empty ("sha" wasn't supplied); 409 "<path> does not
// match <sha>" when SHA is not the file's current blob id (compared without case). A new path
// ignores SHA. The first write into an empty repository creates the root commit and the branch.
func (r *Repo) PutFile(in FileWrite) (FileResult, error) {
	var res FileResult
	if in.Message == "" {
		return res, errStatus(422, "Invalid request.\n\n\"message\" wasn't supplied.")
	}
	if !gitobj.ValidPath(in.Path) {
		return res, errStatus(422, "path contains a malformed path component")
	}
	branch, tip := r.writeTarget(in.Branch)
	if !ValidRefName("refs/heads/" + branch) {
		return res, errStatus(422, "Reference name is invalid")
	}
	root, mode := gitobj.Zero, gitobj.ModeFile
	if tip != nil {
		root = tip.Commit.Tree
	}
	if e, ok := r.Objects.Find(root, in.Path); ok {
		if e.IsDir() {
			return res, errStatus(422, in.Path+" is a directory")
		}
		if in.SHA == "" {
			return res, errStatus(422, "Invalid request.\n\n\"sha\" wasn't supplied.")
		}
		if !strings.EqualFold(in.SHA, e.ID.String()) {
			return res, errStatus(409, in.Path+" does not match "+in.SHA)
		}
		mode = e.Mode
	} else {
		res.Created = true
	}
	now := r.store.Tick()
	blob := r.Objects.Put(gitobj.TypeBlob, in.Data)
	tree, err := r.Objects.SetPath(root, in.Path, mode, blob.ID)
	if err != nil {
		return res, errStatus(422, "A file exists where a directory of "+in.Path+" would be")
	}
	var parents []gitobj.OID
	if tip != nil {
		parents = []gitobj.OID{tip.ID}
	}
	commit := r.commit(tree, parents, in.Message, in.Author, in.Committer, now)
	r.SetRef("refs/heads/"+branch, commit.ID)
	res.Branch, res.Path, res.Mode, res.Blob, res.Size, res.Commit = branch, in.Path, mode, blob.ID, len(in.Data), commit
	return res, nil
}

// DeleteFile is DELETE /contents/{path}: remove one file with one commit. Directories left empty
// disappear; deleting the last file leaves the empty tree.
//
// Errors: 422 for a missing message or SHA; 404 when the repository is empty, the branch does not
// exist or the path is not a file; 409 when SHA is not the file's current blob id.
func (r *Repo) DeleteFile(in FileWrite) (FileResult, error) {
	var res FileResult
	if in.Message == "" {
		return res, errStatus(422, "Invalid request.\n\n\"message\" wasn't supplied.")
	}
	branch := in.Branch
	if branch == "" {
		branch = r.DefaultBranch
	}
	tipID, ok := r.Branch(branch)
	tip := r.Objects.GetCommit(tipID)
	if !ok || tip == nil {
		if r.IsEmpty() {
			return res, errStatus(404, MsgEmptyRepo)
		}
		return res, errStatus(404, "Branch "+branch+" not found")
	}
	path := strings.Trim(in.Path, "/")
	e, ok := r.Objects.Find(tip.Commit.Tree, path)
	if !ok || path == "" || e.IsDir() {
		return res, ErrNotFound
	}
	if in.SHA == "" {
		return res, errStatus(422, "Invalid request.\n\n\"sha\" wasn't supplied.")
	}
	if !strings.EqualFold(in.SHA, e.ID.String()) {
		return res, errStatus(409, path+" does not match "+in.SHA)
	}
	now := r.store.Tick()
	tree, err := r.Objects.RemovePath(tip.Commit.Tree, path)
	if err != nil {
		return res, ErrNotFound
	}
	commit := r.commit(tree, []gitobj.OID{tip.ID}, in.Message, in.Author, in.Committer, now)
	r.SetRef("refs/heads/"+branch, commit.ID)
	res.Branch, res.Path, res.Mode, res.Commit = branch, path, e.Mode, commit
	return res, nil
}
