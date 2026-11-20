package render

import (
	"strconv"

	"forge/internal/gitobj"
	"forge/internal/httpd"
	"forge/internal/store"
)

// UserJSON is the viewer as it appears inside other objects (owner, user, merged_by). It has no
// URLs, so it is a constant.
var UserJSON = []byte(`{"login":"` + store.ViewerLogin + `","id":` + strconv.Itoa(store.ViewerID) +
	`,"node_id":"` + store.ViewerNodeID + `","type":"User","site_admin":false}`)

// AppendViewer appends the full user object of GET /user.
func AppendViewer(dst, host []byte) []byte {
	dst = append(dst, UserJSON[:len(UserJSON)-1]...)
	dst = appendStr(dst, "name", store.ViewerName)
	dst = appendStr(dst, "email", store.ViewerEmail)
	dst = append(appendKey(dst, "html_url"), '"')
	dst = appendOrigin(dst, host)
	dst = append(dst, "/"+store.ViewerLogin+`"`...)
	dst = append(appendKey(dst, "url"), '"')
	dst = appendOrigin(dst, host)
	dst = append(dst, `/api/v3/user"`...)
	return append(dst, '}')
}

// appendRepoURL appends an opening quote and the repository's URL: "<origin>/adaiversen/<repo>" for
// the web, "<origin>/api/v3/repos/adaiversen/<repo>" for the API. The caller appends the rest of the
// URL and the closing quote.
func appendRepoURL(dst, host []byte, api bool, r *store.Repo) []byte {
	dst = append(dst, '"')
	dst = appendOrigin(dst, host)
	if api {
		dst = append(dst, "/api/v3/repos"...)
	}
	dst = append(dst, "/"+store.ViewerLogin+"/"...)
	return append(dst, r.Name...)
}

// AppendRepo appends a repository object.
func AppendRepo(dst, host []byte, r *store.Repo) []byte {
	dst = append(dst, '{')
	dst = appendInt(dst, "id", r.ID)
	dst = appendStr(dst, "node_id", r.NodeID)
	dst = appendStr(dst, "name", r.Name)
	dst = append(appendKey(dst, "full_name"), `"`+store.ViewerLogin+`/`...)
	dst = append(append(dst, r.Name...), '"')
	dst = append(appendKey(dst, "owner"), UserJSON...)
	dst = appendBool(dst, "private", r.Private)
	if r.Private {
		dst = appendStr(dst, "visibility", "private")
	} else {
		dst = appendStr(dst, "visibility", "public")
	}
	dst = appendNullable(dst, "description", r.Description, r.HasDescription)
	dst = appendBool(dst, "fork", false)
	dst = appendStr(dst, "default_branch", r.DefaultBranch)
	dst = append(appendRepoURL(appendKey(dst, "html_url"), host, false, r), '"')
	dst = append(appendRepoURL(appendKey(dst, "url"), host, true, r), '"')
	dst = append(appendRepoURL(appendKey(dst, "clone_url"), host, false, r), `.git"`...)
	dst = appendBool(dst, "has_issues", r.HasIssues)
	dst = appendBool(dst, "archived", r.Archived)
	dst = appendInt(dst, "open_issues_count", int64(r.OpenIssues))
	dst = append(appendKey(dst, "permissions"), `{"admin":true,"push":true,"pull":true}`...)
	dst = appendTimeKey(dst, "created_at", r.CreatedAt)
	dst = appendTimeKey(dst, "updated_at", r.UpdatedAt)
	dst = appendTimeKey(dst, "pushed_at", r.PushedAt)
	return append(dst, '}')
}

// AppendLabel appends a label object.
func AppendLabel(dst, host []byte, l *store.Label) []byte {
	dst = append(dst, '{')
	dst = appendInt(dst, "id", l.ID)
	dst = append(appendKey(dst, "node_id"), '"')
	dst = append(store.AppendNodeID(dst, store.KindLabel, l.ID), '"')
	dst = appendStr(dst, "name", l.Name)
	dst = appendStr(dst, "color", l.Color)
	dst = appendNullable(dst, "description", l.Description, l.HasDescription)
	dst = appendBool(dst, "default", false)
	dst = appendRepoURL(appendKey(dst, "url"), host, true, l.Repo)
	dst = append(dst, "/labels/"...)
	dst = append(appendURLPath(dst, l.Name), '"')
	return append(dst, '}')
}

// AppendLabels appends a JSON array of labels ("[]" when there are none).
func AppendLabels(dst, host []byte, labels []*store.Label) []byte {
	dst = append(dst, '[')
	for i, l := range labels {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = AppendLabel(dst, host, l)
	}
	return append(dst, ']')
}

// appendNumberURL appends `"<repo url>/<kind>/<number>` without the closing quote.
func appendNumberURL(dst, host []byte, api bool, r *store.Repo, kind string, number int64) []byte {
	dst = appendRepoURL(dst, host, api, r)
	dst = append(dst, kind...)
	return strconv.AppendInt(dst, number, 10)
}

// renderIssue appends the issue object. Use AppendIssue or IssueTail, which go through the cache.
func renderIssue(dst, host []byte, is *store.Issue) []byte {
	dst = append(dst, '{')
	dst = appendInt(dst, "id", is.ID)
	dst = append(appendKey(dst, "node_id"), '"')
	dst = append(store.AppendNodeID(dst, store.KindIssue, is.ID), '"')
	dst = appendInt(dst, "number", is.Number)
	dst = appendStr(dst, "title", is.Title)
	dst = appendStr(dst, "body", is.Body)
	if is.Closed {
		dst = appendStr(dst, "state", "closed")
	} else {
		dst = appendStr(dst, "state", "open")
	}
	dst = appendNullable(dst, "state_reason", is.StateReason, is.StateReason != "")
	dst = appendBool(dst, "locked", false)
	dst = AppendLabels(appendKey(dst, "labels"), host, is.Labels)
	dst = append(appendKey(dst, "assignees"), '[', ']')
	dst = appendInt(dst, "comments", int64(len(is.Comments)))
	dst = append(appendKey(dst, "user"), UserJSON...)
	dst = appendStr(dst, "author_association", "OWNER")
	dst = appendTimeKey(dst, "created_at", is.CreatedAt)
	dst = appendTimeKey(dst, "updated_at", is.UpdatedAt)
	dst = appendTimeKey(dst, "closed_at", is.ClosedAt)
	dst = append(appendNumberURL(appendKey(dst, "url"), host, true, is.Repo, "/issues/", is.Number), '"')
	dst = append(appendNumberURL(appendKey(dst, "html_url"), host, false, is.Repo, "/issues/", is.Number), '"')
	dst = append(appendRepoURL(appendKey(dst, "repository_url"), host, true, is.Repo), '"')
	return append(dst, '}')
}

// IssueTail returns the issue as a ready reply tail for httpd.Ctx.JSONTail: Content-Length, blank
// line, JSON. The tail is rendered once and kept on the issue until the issue changes, so reading an
// issue again costs no rendering and no allocation. The returned bytes are shared: never modify them.
func IssueTail(host []byte, is *store.Issue) []byte {
	if tail := is.Wire(host); tail != nil {
		return tail
	}
	var scratch [1024]byte
	body := renderIssue(scratch[:0], host, is)
	tail := httpd.AppendTail(make([]byte, 0, len(body)+32), body)
	is.SetWire(host, tail)
	return tail
}

// AppendIssue appends an issue object (from the same cache IssueTail uses).
func AppendIssue(dst, host []byte, is *store.Issue) []byte {
	return append(dst, httpd.TailBody(IssueTail(host, is))...)
}

// AppendIssues appends a JSON array of issues ("[]" when there are none).
func AppendIssues(dst, host []byte, issues []*store.Issue) []byte {
	dst = append(dst, '[')
	for i, is := range issues {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = AppendIssue(dst, host, is)
	}
	return append(dst, ']')
}

// AppendComment appends an issue comment object.
func AppendComment(dst, host []byte, c *store.Comment) []byte {
	dst = append(dst, '{')
	dst = appendInt(dst, "id", c.ID)
	dst = append(appendKey(dst, "node_id"), '"')
	dst = append(store.AppendNodeID(dst, store.KindComment, c.ID), '"')
	dst = appendStr(dst, "body", c.Body)
	dst = append(appendKey(dst, "user"), UserJSON...)
	dst = appendStr(dst, "author_association", "OWNER")
	dst = appendTimeKey(dst, "created_at", c.CreatedAt)
	dst = appendTimeKey(dst, "updated_at", c.UpdatedAt)
	dst = appendRepoURL(appendKey(dst, "url"), host, true, c.Repo)
	dst = append(dst, "/issues/comments/"...)
	dst = append(strconv.AppendInt(dst, c.ID, 10), '"')
	dst = appendNumberURL(appendKey(dst, "html_url"), host, false, c.Repo, "/issues/", c.Number)
	dst = append(dst, "#issuecomment-"...)
	dst = append(strconv.AppendInt(dst, c.ID, 10), '"')
	dst = append(appendNumberURL(appendKey(dst, "issue_url"), host, true, c.Repo, "/issues/", c.Number), '"')
	return append(dst, '}')
}

// AppendComments appends a JSON array of comments ("[]" when there are none).
func AppendComments(dst, host []byte, comments []*store.Comment) []byte {
	dst = append(dst, '[')
	for i, c := range comments {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = AppendComment(dst, host, c)
	}
	return append(dst, ']')
}

// appendPullSide appends the "head" or "base" object of a pull request.
func appendPullSide(dst, host []byte, r *store.Repo, ref string, id gitobj.OID) []byte {
	dst = append(dst, '{')
	dst = appendStr(dst, "label", store.ViewerLogin+":"+ref)
	dst = appendStr(dst, "ref", ref)
	dst = appendSHA(dst, "sha", id)
	dst = append(appendKey(dst, "user"), UserJSON...)
	dst = AppendRepo(appendKey(dst, "repo"), host, r)
	return append(dst, '}')
}

// AppendPull appends a pull request object. The git-dependent fields (head and base sha, mergeable,
// counts) come from the repository's current refs through Repo.PullInfo.
func AppendPull(dst, host []byte, p *store.Pull) []byte {
	r := p.Repo
	info := r.PullInfo(p)
	dst = append(dst, '{')
	dst = appendInt(dst, "id", p.ID)
	dst = append(appendKey(dst, "node_id"), '"')
	dst = append(store.AppendNodeID(dst, store.KindPull, p.ID), '"')
	dst = appendInt(dst, "number", p.Number)
	if p.Closed {
		dst = appendStr(dst, "state", "closed")
	} else {
		dst = appendStr(dst, "state", "open")
	}
	dst = appendStr(dst, "title", p.Title)
	dst = appendStr(dst, "body", p.Body)
	dst = append(appendKey(dst, "user"), UserJSON...)
	dst = appendBool(dst, "draft", p.Draft)
	dst = appendBool(dst, "locked", false)
	dst = appendPullSide(appendKey(dst, "head"), host, r, p.HeadRef, info.Head)
	dst = appendPullSide(appendKey(dst, "base"), host, r, p.BaseRef, info.Base)
	dst = appendBool(dst, "merged", p.Merged)
	switch info.Mergeable {
	case store.MergeClean:
		dst = appendBool(dst, "mergeable", true)
		dst = appendStr(dst, "mergeable_state", "clean")
	case store.MergeConflicting:
		dst = appendBool(dst, "mergeable", false)
		dst = appendStr(dst, "mergeable_state", "dirty")
	default:
		dst = appendNull(dst, "mergeable")
		dst = appendStr(dst, "mergeable_state", "unknown")
	}
	dst = appendTimeKey(dst, "merged_at", p.MergedAt)
	if p.Merged {
		dst = appendSHA(dst, "merge_commit_sha", p.MergeCommit)
		dst = append(appendKey(dst, "merged_by"), UserJSON...)
	} else {
		dst = appendNull(dst, "merge_commit_sha")
		dst = appendNull(dst, "merged_by")
	}
	dst = appendTimeKey(dst, "created_at", p.CreatedAt)
	dst = appendTimeKey(dst, "updated_at", p.UpdatedAt)
	dst = appendTimeKey(dst, "closed_at", p.ClosedAt)
	dst = appendInt(dst, "comments", int64(len(p.Comments)))
	dst = appendInt(dst, "commits", int64(info.Commits))
	dst = appendInt(dst, "additions", int64(info.Additions))
	dst = appendInt(dst, "deletions", int64(info.Deletions))
	dst = appendInt(dst, "changed_files", int64(len(info.Files)))
	dst = append(appendNumberURL(appendKey(dst, "url"), host, true, r, "/pulls/", p.Number), '"')
	dst = append(appendNumberURL(appendKey(dst, "html_url"), host, false, r, "/pull/", p.Number), '"')
	dst = append(appendNumberURL(appendKey(dst, "issue_url"), host, true, r, "/issues/", p.Number), '"')
	return append(dst, '}')
}

// AppendPulls appends a JSON array of pull requests ("[]" when there are none).
func AppendPulls(dst, host []byte, pulls []*store.Pull) []byte {
	dst = append(dst, '[')
	for i, p := range pulls {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = AppendPull(dst, host, p)
	}
	return append(dst, ']')
}

// AppendMergeResult appends the reply of a successful merge.
func AppendMergeResult(dst []byte, commit gitobj.OID) []byte {
	dst = append(dst, '{')
	dst = appendSHA(dst, "sha", commit)
	dst = appendBool(dst, "merged", true)
	dst = appendStr(dst, "message", store.MsgMerged)
	return append(dst, '}')
}

// DocURL is the documentation_url of every REST error body that has no more specific one.
const DocURL = "https://docs.github.com/rest"

// AppendError appends the REST error body of a store error: message, the errors array when the
// error carries a validation entry, and documentation_url.
func AppendError(dst []byte, e *store.Error) []byte {
	dst = append(dst, '{')
	dst = appendStr(dst, "message", e.Message)
	if e.Code != "" {
		dst = append(appendKey(dst, "errors"), '[', '{')
		if e.Resource != "" {
			dst = appendStr(dst, "resource", e.Resource)
		}
		if e.Field != "" {
			dst = appendStr(dst, "field", e.Field)
		}
		dst = appendStr(dst, "code", e.Code)
		if e.Value != "" {
			dst = appendStr(dst, "value", e.Value)
		}
		if e.Detail != "" {
			dst = appendStr(dst, "message", e.Detail)
		}
		dst = append(dst, '}', ']')
	}
	if e.DocURL != "" {
		dst = appendStr(dst, "documentation_url", e.DocURL)
	} else {
		dst = appendStr(dst, "documentation_url", DocURL)
	}
	return append(dst, '}')
}

// AppendMessage appends the REST error body for a bare message: {"message":..,"documentation_url":..}.
func AppendMessage(dst []byte, message string) []byte {
	dst = append(dst, '{')
	dst = appendStr(dst, "message", message)
	dst = appendStr(dst, "documentation_url", DocURL)
	return append(dst, '}')
}
