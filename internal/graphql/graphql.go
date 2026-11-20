// Package graphql is the GraphQL v4 surface at POST /api/graphql. It resolves every field from the
// same store objects REST renders, and its mutations call the same store operations.
//
// This file is the registration point: transport, authentication and the reply helpers. The parser,
// schema registry and executor are filled in by the GraphQL change.
package graphql

import (
	"forge/internal/httpd"
	"forge/internal/store"
)

// API serves /api/graphql.
type API struct {
	st *store.Store
}

// New returns the GraphQL surface over st.
func New(st *store.Store) *API { return &API{st: st} }

var (
	notFound       = []byte(`{"message":"Not Found"}`)
	unauthorized   = []byte(`{"message":"This endpoint requires you to be authenticated.","documentation_url":"https://docs.github.com/graphql/guides/forming-calls-with-graphql#authenticating-with-graphql"}`)
	badCredentials = []byte(`{"message":"Bad credentials","documentation_url":"https://docs.github.com/graphql"}`)
	notImplemented = []byte(`{"errors":[{"message":"GraphQL is not implemented yet"}]}`)
)

// Serve answers one request to /api/graphql. The store is locked by the caller. Authentication is
// decided before the body is looked at.
func (a *API) Serve(c *httpd.Ctx) {
	if c.Method != httpd.POST {
		c.JSON(404, notFound)
		return
	}
	switch c.Auth() {
	case httpd.AuthNone:
		c.JSON(401, unauthorized)
		return
	case httpd.AuthBad:
		c.JSON(401, badCredentials)
		return
	}
	c.JSON(200, notImplemented)
}
