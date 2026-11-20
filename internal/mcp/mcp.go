// Package mcp is the Model Context Protocol surface at POST /mcp: JSON-RPC over plain JSON
// responses, six tools that call the same store operations and renderers as REST.
//
// This file is the registration point: method and authentication gates and the JSON-RPC error
// reply. The methods and tools are filled in by the MCP change.
package mcp

import (
	"forge/internal/httpd"
	"forge/internal/store"
)

// Server serves /mcp.
type Server struct {
	st *store.Store
}

// New returns the MCP surface over st.
func New(st *store.Store) *Server { return &Server{st: st} }

var (
	methodNotAllowed = []byte(`{"message":"Method Not Allowed"}`)
	unauthorized     = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"Requires authentication"}}`)
	notImplemented   = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32601,"message":"MCP is not implemented yet"}}`)
)

// Serve answers one request to /mcp. The store is locked by the caller. The order is fixed: HTTP
// method, then authentication (before the body is read as JSON), then the JSON-RPC envelope.
func (s *Server) Serve(c *httpd.Ctx) {
	if c.Method != httpd.POST {
		c.AddHeader("Allow", "POST")
		c.JSON(405, methodNotAllowed)
		return
	}
	if c.Auth() != httpd.AuthOK {
		c.AddHeader("WWW-Authenticate", "Bearer")
		c.JSON(401, unauthorized)
		return
	}
	c.JSON(200, notImplemented)
}
