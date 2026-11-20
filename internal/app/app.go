// Package app wires the forge together: one store, the four surfaces over it, and the dispatcher
// that sends each request to its surface.
package app

import (
	"bytes"

	"forge/internal/githttp"
	"forge/internal/graphql"
	"forge/internal/httpd"
	"forge/internal/mcp"
	"forge/internal/rest"
	"forge/internal/store"
)

// App is a complete forge without its network listener.
type App struct {
	Store   *store.Store
	REST    *rest.API
	GraphQL *graphql.API
	MCP     *mcp.Server
	Git     *githttp.Server
}

// New builds an empty forge.
func New() *App {
	st := store.New()
	return &App{
		Store:   st,
		REST:    rest.New(st),
		GraphQL: graphql.New(st),
		MCP:     mcp.New(st),
		Git:     githttp.New(st),
	}
}

var (
	healthy  = []byte(`{"ok":true}`)
	notFound = []byte(`{"message":"Not Found"}`)
)

// Serve is the root handler: it picks the surface by path prefix and runs it with the store locked.
//
//	/api/v3, /api/v3/...        REST
//	/{owner}/{repo}.git/...     git smart HTTP
//	/api/graphql                GraphQL
//	/mcp                        MCP
//	/healthz                    liveness probe, no token
//	anything else               404 {"message":"Not Found"}
//
// The store mutex is held for the whole surface call and released before the engine writes the
// response, so no surface ever locks and none can be observed mid-update.
func (a *App) Serve(c *httpd.Ctx) {
	p := c.Path
	if len(p) >= 7 && string(p[:7]) == "/api/v3" && (len(p) == 7 || p[7] == '/') {
		a.Store.Lock()
		defer a.Store.Unlock()
		a.REST.Serve(c, p[7:])
		return
	}
	if owner, repo, rest, ok := splitGitPath(p); ok {
		a.Store.Lock()
		defer a.Store.Unlock()
		a.Git.Serve(c, owner, repo, rest)
		return
	}
	switch string(p) {
	case "/api/graphql":
		a.Store.Lock()
		defer a.Store.Unlock()
		a.GraphQL.Serve(c)
	case "/mcp":
		a.Store.Lock()
		defer a.Store.Unlock()
		a.MCP.Serve(c)
	case "/healthz":
		c.JSON(200, healthy)
	default:
		c.JSON(404, notFound)
	}
}

// splitGitPath recognises "/{owner}/{repo}.git[/rest]" and returns its parts; rest keeps its
// leading slash.
func splitGitPath(p []byte) (owner, repo, rest []byte, ok bool) {
	if len(p) < 2 || p[0] != '/' {
		return nil, nil, nil, false
	}
	i := bytes.IndexByte(p[1:], '/')
	if i <= 0 {
		return nil, nil, nil, false
	}
	owner = p[1 : 1+i]
	repo = p[2+i:]
	if j := bytes.IndexByte(repo, '/'); j >= 0 {
		repo, rest = repo[:j], repo[j:]
	}
	if len(repo) <= 4 || string(repo[len(repo)-4:]) != ".git" {
		return nil, nil, nil, false
	}
	return owner, repo[:len(repo)-4], rest, true
}
