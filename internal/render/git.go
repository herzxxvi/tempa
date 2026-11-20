package render

import (
	"encoding/base64"
	"strings"

	"forge/internal/gitobj"
	"forge/internal/store"
)

// Renderers for the git-backed REST objects: branches, refs, commits, contents.

// appendCommitURL appends `"<api repo url>/commits/<sha>"`.
func appendCommitURL(dst, host []byte, r *store.Repo, id gitobj.OID) []byte {
	dst = appendRepoURL(dst, host, true, r)
	dst = append(dst, "/commits/"...)
	return append(id.AppendHex(dst), '"')
}

// AppendBranch appends a branch object: {"name", "commit": {"sha", "url"}, "protected"}. ref is the
// full ref as Repo.Branches returns it.
func AppendBranch(dst, host []byte, r *store.Repo, ref store.Ref) []byte {
	dst = append(dst, '{')
	dst = appendStr(dst, "name", strings.TrimPrefix(ref.Name, "refs/heads/"))
	dst = append(appendKey(dst, "commit"), '{')
	dst = appendSHA(dst, "sha", ref.ID)
	dst = appendCommitURL(appendKey(dst, "url"), host, r, ref.ID)
	dst = append(dst, '}')
	dst = appendBool(dst, "protected", false)
	return append(dst, '}')
}

// AppendGitRef appends a git ref object: {"ref", "url", "object": {"sha", "type", "url"}}.
func AppendGitRef(dst, host []byte, r *store.Repo, ref store.Ref) []byte {
	typ := "commit"
	if o := r.Objects.Get(ref.ID); o != nil {
		typ = o.Type.String()
	}
	dst = append(dst, '{')
	dst = appendStr(dst, "ref", ref.Name)
	dst = appendRepoURL(appendKey(dst, "url"), host, true, r)
	dst = append(dst, "/git/"...)
	dst = append(appendURLPath(dst, ref.Name), '"')
	dst = append(appendKey(dst, "object"), '{')
	dst = appendSHA(dst, "sha", ref.ID)
	dst = appendStr(dst, "type", typ)
	dst = appendRepoURL(appendKey(dst, "url"), host, true, r)
	dst = append(dst, "/git/"...)
	dst = append(dst, typ...)
	dst = append(dst, "s/"...)
	dst = append(ref.ID.AppendHex(dst), '"')
	dst = append(dst, '}')
	return append(dst, '}')
}

func appendSignature(dst []byte, key string, s *gitobj.Signature) []byte {
	dst = append(appendKey(dst, key), '{')
	dst = appendStr(dst, "name", s.Name)
	dst = appendStr(dst, "email", s.Email)
	dst = appendTimeKey(dst, "date", s.When)
	return append(dst, '}')
}

func appendParents(dst []byte, parents []gitobj.OID) []byte {
	dst = append(appendKey(dst, "parents"), '[')
	for i, p := range parents {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '{')
		dst = appendSHA(dst, "sha", p)
		dst = append(dst, '}')
	}
	return append(dst, ']')
}

// commitMessage is a commit's message as the API shows it: at most one trailing newline removed, so
// a message sent through the API comes back exactly and a pushed commit loses git's final newline.
func commitMessage(c *gitobj.Commit) string {
	return strings.TrimSuffix(c.Message, "\n")
}

// appendActor appends the viewer when the signature is the viewer's, else null.
func appendActor(dst []byte, key string, s *gitobj.Signature) []byte {
	if s.Email == store.ViewerEmail {
		return append(appendKey(dst, key), UserJSON...)
	}
	return appendNull(dst, key)
}

// AppendCommit appends a commit as the commits API lists it:
// {"sha", "commit": {"message", "tree", "author", "committer"}, "parents", "author", "committer", ...}.
func AppendCommit(dst, host []byte, r *store.Repo, o *gitobj.Object) []byte {
	c, _ := gitobj.ParseCommit(o.Data)
	dst = append(dst, '{')
	dst = appendSHA(dst, "sha", o.ID)
	dst = append(appendKey(dst, "commit"), '{')
	dst = appendStr(dst, "message", commitMessage(&c))
	dst = append(appendKey(dst, "tree"), '{')
	dst = appendSHA(dst, "sha", c.Tree)
	dst = append(dst, '}')
	dst = appendSignature(dst, "author", &c.Author)
	dst = appendSignature(dst, "committer", &c.Committer)
	dst = append(dst, '}')
	dst = appendParents(dst, c.Parents)
	dst = appendActor(dst, "author", &c.Author)
	dst = appendActor(dst, "committer", &c.Committer)
	dst = appendCommitURL(appendKey(dst, "url"), host, r, o.ID)
	dst = appendRepoURL(appendKey(dst, "html_url"), host, false, r)
	dst = append(dst, "/commit/"...)
	dst = append(o.ID.AppendHex(dst), '"')
	return append(dst, '}')
}

// appendGitCommit appends the flat commit object of a contents write reply:
// {"sha", "message", "tree", "parents", "author", "committer", "url", "html_url"}.
func appendGitCommit(dst, host []byte, r *store.Repo, o *gitobj.Object) []byte {
	c, _ := gitobj.ParseCommit(o.Data)
	dst = append(dst, '{')
	dst = appendSHA(dst, "sha", o.ID)
	dst = appendStr(dst, "message", commitMessage(&c))
	dst = append(appendKey(dst, "tree"), '{')
	dst = appendSHA(dst, "sha", c.Tree)
	dst = append(dst, '}')
	dst = appendParents(dst, c.Parents)
	dst = appendSignature(dst, "author", &c.Author)
	dst = appendSignature(dst, "committer", &c.Committer)
	dst = appendRepoURL(appendKey(dst, "url"), host, true, r)
	dst = append(dst, "/git/commits/"...)
	dst = append(o.ID.AppendHex(dst), '"')
	dst = appendRepoURL(appendKey(dst, "html_url"), host, false, r)
	dst = append(dst, "/commit/"...)
	dst = append(o.ID.AppendHex(dst), '"')
	return append(dst, '}')
}

func entryType(mode uint32) string {
	switch mode {
	case gitobj.ModeDir:
		return "dir"
	case gitobj.ModeSymlink:
		return "symlink"
	case gitobj.ModeGitlink:
		return "submodule"
	}
	return "file"
}

// appendEntryMeta appends the keys every contents object has, without braces: type, size, name,
// path, sha and the URLs. commit is the commit the entry was read from; it pins the web URLs.
func appendEntryMeta(dst, host []byte, r *store.Repo, commit gitobj.OID, path string, mode uint32, id gitobj.OID, size int) []byte {
	name := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		name = path[i+1:]
	}
	typ := entryType(mode)
	dst = appendStr(dst, "type", typ)
	dst = appendInt(dst, "size", int64(size))
	dst = appendStr(dst, "name", name)
	dst = appendStr(dst, "path", path)
	dst = appendSHA(dst, "sha", id)
	dst = appendRepoURL(appendKey(dst, "url"), host, true, r)
	dst = append(dst, "/contents/"...)
	dst = append(appendURLPath(dst, path), '"')
	dst = appendRepoURL(appendKey(dst, "git_url"), host, true, r)
	if typ == "dir" {
		dst = append(dst, "/git/trees/"...)
	} else {
		dst = append(dst, "/git/blobs/"...)
	}
	dst = append(id.AppendHex(dst), '"')
	dst = appendRepoURL(appendKey(dst, "html_url"), host, false, r)
	if typ == "dir" {
		dst = append(dst, "/tree/"...)
	} else {
		dst = append(dst, "/blob/"...)
	}
	dst = commit.AppendHex(dst)
	dst = append(dst, '/')
	dst = append(appendURLPath(dst, path), '"')
	if typ != "file" {
		return appendNull(dst, "download_url")
	}
	dst = appendRepoURL(appendKey(dst, "download_url"), host, false, r)
	dst = append(dst, "/raw/"...)
	dst = commit.AppendHex(dst)
	dst = append(dst, '/')
	return append(appendURLPath(dst, path), '"')
}

// AppendContent appends the answer of the contents API for a path: a file object with its content
// in unwrapped standard base64, or an array of directory entries sorted by name. path is the
// requested path without leading or trailing slashes ("" for the root).
func AppendContent(dst, host []byte, r *store.Repo, path string, c *store.Content) []byte {
	if !c.IsDir {
		dst = append(dst, '{')
		dst = appendEntryMeta(dst, host, r, c.Commit.ID, path, c.Mode, c.ID, len(c.Data))
		dst = appendStr(dst, "encoding", "base64")
		dst = append(appendKey(dst, "content"), '"')
		dst = base64.StdEncoding.AppendEncode(dst, c.Data)
		return append(dst, '"', '}')
	}
	dst = append(dst, '[')
	for i := range c.Entries {
		e := &c.Entries[i]
		if i > 0 {
			dst = append(dst, ',')
		}
		size := 0
		if !e.IsDir() {
			if blob := r.Objects.Get(e.ID); blob != nil {
				size = len(blob.Data)
			}
		}
		full := e.Name
		if path != "" {
			full = path + "/" + e.Name
		}
		dst = append(dst, '{')
		dst = appendEntryMeta(dst, host, r, c.Commit.ID, full, e.Mode, e.ID, size)
		dst = append(dst, '}')
	}
	return append(dst, ']')
}

// AppendFileResult appends the reply of a contents write: {"content": {...} | null, "commit": {...}}.
// The content object carries "encoding":"base64" (clients of this forge expect the key) but not the
// content itself. deleted selects the DELETE shape with "content": null.
func AppendFileResult(dst, host []byte, r *store.Repo, res *store.FileResult, deleted bool) []byte {
	dst = append(dst, '{')
	if deleted {
		dst = appendNull(dst, "content")
	} else {
		dst = append(appendKey(dst, "content"), '{')
		dst = appendEntryMeta(dst, host, r, res.Commit.ID, res.Path, res.Mode, res.Blob, res.Size)
		dst = appendStr(dst, "encoding", "base64")
		dst = append(dst, '}')
	}
	dst = appendGitCommit(appendKey(dst, "commit"), host, r, res.Commit)
	return append(dst, '}')
}

// AppendPullFiles appends the array of the pull request files API: one object per changed file
// with filename, status, additions, deletions, changes, sha and the content URLs.
func AppendPullFiles(dst, host []byte, r *store.Repo, info *store.PullInfo, files []store.FileChange) []byte {
	dst = append(dst, '[')
	for i := range files {
		f := &files[i]
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '{')
		dst = appendSHA(dst, "sha", f.Blob)
		dst = appendStr(dst, "filename", f.Path)
		dst = appendStr(dst, "status", f.Status)
		dst = appendInt(dst, "additions", int64(f.Additions))
		dst = appendInt(dst, "deletions", int64(f.Deletions))
		dst = appendInt(dst, "changes", int64(f.Additions+f.Deletions))
		dst = appendRepoURL(appendKey(dst, "blob_url"), host, false, r)
		dst = append(dst, "/blob/"...)
		dst = info.Head.AppendHex(dst)
		dst = append(dst, '/')
		dst = append(appendURLPath(dst, f.Path), '"')
		dst = appendRepoURL(appendKey(dst, "raw_url"), host, false, r)
		dst = append(dst, "/raw/"...)
		dst = info.Head.AppendHex(dst)
		dst = append(dst, '/')
		dst = append(appendURLPath(dst, f.Path), '"')
		dst = appendRepoURL(appendKey(dst, "contents_url"), host, true, r)
		dst = append(dst, "/contents/"...)
		dst = appendURLPath(dst, f.Path)
		dst = append(dst, "?ref="...)
		dst = append(info.Head.AppendHex(dst), '"')
		dst = append(dst, '}')
	}
	return append(dst, ']')
}
