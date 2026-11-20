// Package githttp is the git smart-HTTP surface: info/refs advertisements, upload-pack (protocol
// v2) and receive-pack, over the same store and object database as every other surface.
//
// This file is the registration point only; the protocol code arrives in a later change.
package githttp

import (
	"forge/internal/httpd"
	"forge/internal/store"
)

// Server serves /{owner}/{repo}.git/...
type Server struct {
	st *store.Store
}

// New returns the git surface over st.
func New(st *store.Store) *Server { return &Server{st: st} }

var (
	notFound       = []byte(`{"message":"Not Found"}`)
	notImplemented = []byte(`{"message":"git smart HTTP is not implemented yet"}`)
)

// Serve answers one request below /{owner}/{repo}.git. sub is the path after ".git" ("/info/refs",
// "/git-upload-pack", "/git-receive-pack"). The store is locked by the caller. A repository that
// does not exist, or an owner other than the viewer, is a JSON 404.
func (s *Server) Serve(c *httpd.Ctx, owner, repo, sub []byte) {
	if s.st.Lookup(owner, repo) == nil {
		c.JSON(404, notFound)
		return
	}
	c.JSON(501, notImplemented)
}
