// Package githttp is the git smart-HTTP surface: info/refs advertisements, upload-pack (protocol v2,
// with a basic v0 for old clients) and receive-pack, over the same store and object database as
// every other surface.
//
// Speed comes from doing each piece of work once:
//   - the v2 capability advertisement is one constant response for every repository;
//   - the receive-pack advertisement is rendered once per ref change and kept as a whole response;
//   - ls-refs and clone-shaped fetch responses are kept per (repository, ref state, request bytes);
//     the store mutex makes that single-flight, so fifty identical concurrent clones build one pack;
//   - a push costs what its pack costs: its objects are inflated, hashed and checked against the
//     objects they name, the fast-forward test looks at the new commit's parents first, and nothing
//     is derived from history;
//   - pushed objects keep their own zlib streams, which go out again verbatim.
//
// Everything here runs with the store mutex held (see internal/app), so the Server's scratch buffers
// are never used by two requests at once.
package githttp

import (
	"bytes"

	"forge/internal/httpd"
	"forge/internal/store"
)

const (
	typeUploadAdvert  = "application/x-git-upload-pack-advertisement"
	typeReceiveAdvert = "application/x-git-receive-pack-advertisement"
	typeUploadResult  = "application/x-git-upload-pack-result"
	typeReceiveResult = "application/x-git-receive-pack-result"
)

// Server serves /{owner}/{repo}.git/...
type Server struct {
	st *store.Store

	// Scratch space, reused by every request.
	up    unpacker
	cmds  []command
	body  []byte // response body under construction
	pack  []byte // pack, or push report, under construction
	fetch fetchState
}

// New returns the git surface over st.
func New(st *store.Store) *Server { return &Server{st: st} }

var (
	notFound     = []byte(`{"message":"Not Found"}`)
	unauthorized = []byte(`{"message":"Requires authentication"}`)
	badRequest   = []byte(`{"message":"Malformed git request"}`)
)

// Serve answers one request below /{owner}/{repo}.git; sub is the path after ".git". The store is
// locked by the caller.
//
//	GET  /info/refs?service=git-upload-pack     capability (v2) or ref (v0) advertisement
//	GET  /info/refs?service=git-receive-pack    ref advertisement, always v0
//	POST /git-upload-pack                       ls-refs and fetch
//	POST /git-receive-pack                      push
//
// A request without the token is a 401 that asks for Basic credentials (git clients send the token
// as the password). A repository that does not exist, an owner other than the viewer, an unknown
// service or path: a JSON 404. Nothing here redirects and nothing answers 5xx.
func (s *Server) Serve(c *httpd.Ctx, owner, repo, sub []byte) {
	if c.Auth() != httpd.AuthOK {
		c.AddHeader("WWW-Authenticate", `Basic realm="forge"`)
		c.JSON(401, unauthorized)
		return
	}
	r := s.st.Lookup(owner, repo)
	if r == nil {
		c.JSON(404, notFound)
		return
	}
	switch string(sub) {
	case "/info/refs":
		if c.Method == httpd.GET {
			s.infoRefs(c, r)
			return
		}
	case "/git-upload-pack":
		if c.Method == httpd.POST {
			s.uploadPack(c, r)
			return
		}
	case "/git-receive-pack":
		if c.Method == httpd.POST {
			s.receivePack(c, r)
			return
		}
	}
	c.JSON(404, notFound)
}

func (s *Server) infoRefs(c *httpd.Ctx, r *store.Repo) {
	service, _ := c.QueryRaw("service")
	switch string(service) {
	case "git-upload-pack":
		if wantsV2(c.GitProtocol) {
			c.Prebuilt(advertV2)
		} else {
			c.Prebuilt(s.advertUploadV0(r))
		}
	case "git-receive-pack":
		// Push is protocol v0 whatever Git-Protocol says.
		c.Prebuilt(s.advertReceive(r))
	default:
		c.JSON(404, notFound)
	}
}

// wantsV2 reports whether a Git-Protocol header value ("version=2", possibly one of several
// colon-separated entries) asks for protocol version 2.
func wantsV2(value []byte) bool {
	if string(value) == "version=2" {
		return true
	}
	for len(value) > 0 {
		part := value
		if i := bytes.IndexByte(value, ':'); i >= 0 {
			part, value = value[:i], value[i+1:]
		} else {
			value = nil
		}
		if string(bytes.TrimSpace(part)) == "version=2" {
			return true
		}
	}
	return false
}
