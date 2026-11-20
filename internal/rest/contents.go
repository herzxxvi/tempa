package rest

import (
	"encoding/base64"

	"forge/internal/httpd"
	"forge/internal/render"
	"forge/internal/store"
)

// The contents API: read a file or a directory at a revision, and create, update or delete one file
// with one commit. The store works on raw bytes; base64 is this surface's business.

// contentsPath returns the decoded {path...} of a contents route, "" for the route without one.
func contentsPath(c *httpd.Ctx) (path string, ok bool) {
	if c.Param(2) == nil {
		return "", true
	}
	return restParam(c, 2)
}

// getContents is GET /contents[/{path}][?ref=<branch, tag or commit>]: a file object with its
// content in base64, or an array of directory entries sorted by name. All failures are 404s: the
// repository is empty, the ref names no commit, the path does not exist.
func (a *API) getContents(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	path, ok := contentsPath(c)
	if !ok {
		return
	}
	content, err := r.Contents(path, c.QueryString("ref"))
	if err != nil {
		Fail(c, err)
		return
	}
	c.Scratch = render.AppendContent(c.Scratch[:0], c.Host, r, path, &content)
	c.JSON(200, c.Scratch)
}

// identInput is the optional "author" / "committer" of a contents write.
type identInput struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (in *identInput) ident() *store.Ident {
	if in == nil {
		return nil
	}
	return &store.Ident{Name: in.Name, Email: in.Email}
}

// fileInput is the body of PUT and DELETE /contents/{path}.
type fileInput struct {
	Message   string      `json:"message"`
	Content   *string     `json:"content"`
	SHA       string      `json:"sha"`
	Branch    string      `json:"branch"`
	Author    *identInput `json:"author"`
	Committer *identInput `json:"committer"`
}

func (in *fileInput) write(path string) store.FileWrite {
	return store.FileWrite{
		Path: path, Message: in.Message, SHA: in.SHA, Branch: in.Branch,
		Author: in.Author.ident(), Committer: in.Committer.ident(),
	}
}

// decodeBase64 decodes standard base64 after dropping whitespace (clients wrap it in lines), with or
// without padding.
func decodeBase64(s string) ([]byte, bool) {
	packed := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if ch := s[i]; ch != '\n' && ch != '\r' && ch != ' ' && ch != '\t' {
			packed = append(packed, ch)
		}
	}
	for len(packed) > 0 && packed[len(packed)-1] == '=' {
		packed = packed[:len(packed)-1]
	}
	data := make([]byte, base64.RawStdEncoding.DecodedLen(len(packed)))
	n, err := base64.RawStdEncoding.Decode(data, packed)
	return data[:n], err == nil
}

// putContents is PUT /contents/{path}: 201 when the commit created the file, 200 when it updated
// it. The first write into an empty repository creates its default branch. Updating needs the
// current blob sha: without it 422, with a stale one 409.
func (a *API) putContents(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	// The path goes to the store as it was sent: a leading or trailing slash is its 422.
	path, ok := c.ParamString(2)
	if !ok {
		NotFound(c)
		return
	}
	var in fileInput
	if !Decode(c, &in) {
		return
	}
	if in.Content == nil {
		Message(c, 422, "Invalid request.\n\n\"content\" wasn't supplied.")
		return
	}
	w := in.write(path)
	if w.Data, ok = decodeBase64(*in.Content); !ok {
		Message(c, 422, "content is not valid Base64")
		return
	}
	res, err := r.PutFile(w)
	if err != nil {
		Fail(c, err)
		return
	}
	c.Scratch = render.AppendFileResult(c.Scratch[:0], c.Host, r, &res, false)
	if res.Created {
		c.JSON(201, c.Scratch)
	} else {
		c.JSON(200, c.Scratch)
	}
}

// deleteContents is DELETE /contents/{path}: one commit that removes the file; 200 with
// {"content": null, "commit": {...}}. message, sha and branch come in the JSON body or, as some
// clients send them on a DELETE, in the query.
func (a *API) deleteContents(c *httpd.Ctx) {
	r := a.Repo(c, 0)
	if r == nil {
		return
	}
	path, ok := contentsPath(c)
	if !ok {
		return
	}
	var in fileInput
	if !DecodeOptional(c, &in) {
		return
	}
	if in.Message == "" {
		in.Message = c.QueryString("message")
	}
	if in.SHA == "" {
		in.SHA = c.QueryString("sha")
	}
	if in.Branch == "" {
		in.Branch = c.QueryString("branch")
	}
	res, err := r.DeleteFile(in.write(path))
	if err != nil {
		Fail(c, err)
		return
	}
	c.Scratch = render.AppendFileResult(c.Scratch[:0], c.Host, r, &res, true)
	c.JSON(200, c.Scratch)
}
