// Package mcp is the Model Context Protocol surface at POST /mcp: JSON-RPC 2.0 over the Streamable
// HTTP transport, always answered with one plain JSON body (never an event stream, never a session).
//
// It speaks revision 2026-07-28, where server/discover describes the server in one request and
// nothing has to precede any other request, and it also answers the older initialize handshake.
// Its six tools call the same store operations as the REST API and return the bytes of the same
// renderers, so an object made here is the object REST reads back.
//
// This file is the transport and the JSON-RPC layer; tools.go holds the tools.
package mcp

import (
	"encoding/json"
	"os"
	"runtime/debug"
	"strconv"
	"unicode/utf8"

	"forge/internal/httpd"
	"forge/internal/render"
	"forge/internal/store"
)

// Server serves /mcp.
type Server struct {
	st *store.Store
}

// New returns the MCP surface over st.
func New(st *store.Store) *Server { return &Server{st: st} }

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Protocol revisions. The modern one needs no handshake; the legacy ones open with initialize.
const (
	modernRevision = "2026-07-28"
	latestLegacy   = "2025-11-25"
)

var legacyRevisions = [...]string{latestLegacy, "2025-06-18", "2025-03-26", "2024-11-05"}

// The parts of the server's self-description shared by server/discover and initialize.
const (
	serverInfo   = `{"name":"forge-mcp","version":"1.0.0"}`
	capabilities = `{"tools":{"listChanged":false}}`
	instructions = `"GitHub-compatible forge. Tools act on the same store as /api/v3 and /api/graphql."`
)

var (
	methodNotAllowed = []byte(`{"message":"Method Not Allowed"}`)
	unauthorized     = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"Requires authentication"}}`)
	badCredentials   = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"Bad credentials"}}`)
	nullID           = []byte("null")
	emptyResult      = []byte("{}")

	// discoverResult is the whole answer to server/discover. The identity appears twice because
	// 2026-07-28 clients read it from _meta and older tooling from the top level.
	discoverResult = []byte(`{"supportedVersions":["` + modernRevision + `","` + legacyRevisions[0] + `","` + legacyRevisions[1] + `","` + legacyRevisions[2] + `","` + legacyRevisions[3] + `"]` +
		`,"capabilities":` + capabilities +
		`,"serverInfo":` + serverInfo +
		`,"instructions":` + instructions +
		`,"_meta":{"io.modelcontextprotocol/serverInfo":` + serverInfo + `}}`)
)

// Serve answers one request to /mcp. The store is locked by the caller. The order is fixed: HTTP
// method, then authentication (before the body is looked at), then the JSON-RPC message.
func (s *Server) Serve(c *httpd.Ctx) {
	if c.Method != httpd.POST {
		c.AddHeader("Allow", "POST")
		c.JSON(405, methodNotAllowed)
		return
	}
	switch c.Auth() {
	case httpd.AuthNone:
		c.AddHeader("WWW-Authenticate", "Bearer")
		c.JSON(401, unauthorized)
		return
	case httpd.AuthBad:
		c.AddHeader("WWW-Authenticate", "Bearer")
		c.JSON(401, badCredentials)
		return
	}
	status, body := s.answer(c)
	if body == nil {
		c.Empty(status) // a notification: accepted, nothing to say
		return
	}
	c.JSON(status, body)
}

// message is a JSON-RPC message with every member left undecoded: the id must be echoed exactly as
// it was sent, and whether it is present at all tells a request from a notification.
type message struct {
	ID     json.RawMessage `json:"id"`
	Method json.RawMessage `json:"method"`
	Params json.RawMessage `json:"params"`
}

// answer reads the JSON-RPC message in the request body and returns the HTTP status and body of the
// reply; a nil body means "no body". The body is built in c.Scratch.
//
// No protocol header, _meta entry or earlier request is required: validating what a client may
// legitimately omit only turns working clients away.
func (s *Server) answer(c *httpd.Ctx) (status int, body []byte) {
	id := nullID
	defer func() {
		// A bug in a tool must not cost the client its answer: report it as a JSON-RPC internal
		// error under the request's id instead of the engine's bare 500.
		if p := recover(); p != nil {
			reportPanic(p)
			c.Scratch = appendError(c.Scratch[:0], id, codeInternalError, "Internal error")
			status, body = 200, c.Scratch
		}
	}()

	text := trimSpace(c.Body)
	if len(text) == 0 || !json.Valid(text) {
		return s.fail(c, 400, id, codeParseError, "Parse error")
	}
	var msg message
	if text[0] != '{' || json.Unmarshal(text, &msg) != nil {
		return s.fail(c, 400, id, codeInvalidRequest, "Invalid Request")
	}
	hasID := msg.ID != nil
	if hasID {
		if !validID(msg.ID) {
			return s.fail(c, 400, id, codeInvalidRequest, "Invalid Request")
		}
		id = msg.ID
	}
	var method string
	if json.Unmarshal(msg.Method, &method) != nil || method == "" {
		return s.fail(c, 400, id, codeInvalidRequest, "Invalid Request")
	}
	if !hasID {
		return 202, nil
	}

	switch method {
	case "server/discover":
		return s.result(c, id, discoverResult)
	case "initialize":
		return s.initialize(c, id, msg.Params)
	case "ping":
		return s.result(c, id, emptyResult)
	case "tools/list":
		return s.result(c, id, toolsListResult)
	case "tools/call":
		return s.callTool(c, id, msg.Params)
	}
	return s.fail(c, 200, id, codeMethodNotFound, "Method not found: "+method)
}

// result replies with a success whose result is the given JSON.
func (s *Server) result(c *httpd.Ctx, id, result []byte) (int, []byte) {
	b := appendHead(c.Scratch[:0], id)
	b = append(b, `,"result":`...)
	b = append(b, result...)
	b = append(b, '}')
	c.Scratch = b
	return 200, b
}

// fail replies with a JSON-RPC error.
func (s *Server) fail(c *httpd.Ctx, status int, id []byte, code int, text string) (int, []byte) {
	c.Scratch = appendError(c.Scratch[:0], id, code, text)
	return status, c.Scratch
}

// initialize answers the handshake of the revisions before 2026-07-28. The server agrees to the
// revision the client offers when it is one of those, and otherwise proposes the newest of them.
// Nothing is remembered: later requests are served whether or not this one ever came.
func (s *Server) initialize(c *httpd.Ctx, id []byte, params json.RawMessage) (int, []byte) {
	var p struct {
		ProtocolVersion json.RawMessage `json:"protocolVersion"`
	}
	var offered string
	if json.Unmarshal(params, &p) == nil {
		json.Unmarshal(p.ProtocolVersion, &offered) // not a string: offered stays empty
	}
	revision := latestLegacy
	for _, known := range legacyRevisions {
		if offered == known {
			revision = known
		}
	}
	b := appendHead(c.Scratch[:0], id)
	b = append(b, `,"result":{"protocolVersion":"`...)
	b = append(b, revision...)
	b = append(b, `","capabilities":`+capabilities+`,"serverInfo":`+serverInfo+`,"instructions":`+instructions+`}}`...)
	c.Scratch = b
	return 200, b
}

// appendHead appends the opening of a response: {"jsonrpc":"2.0","id":<id>. The id is the client's
// own bytes, so a string stays that string and a number that number.
func appendHead(dst, id []byte) []byte {
	dst = append(dst, `{"jsonrpc":"2.0","id":`...)
	return append(dst, id...)
}

// appendError appends a complete JSON-RPC error response.
func appendError(dst, id []byte, code int, text string) []byte {
	dst = appendHead(dst, id)
	dst = append(dst, `,"error":{"code":`...)
	dst = strconv.AppendInt(dst, int64(code), 10)
	dst = append(dst, `,"message":`...)
	dst = render.AppendString(dst, text)
	return append(dst, '}', '}')
}

// validID reports whether raw, a valid JSON value, may be a JSON-RPC id: a string, a number or null.
// The id is echoed byte for byte, so a string that is not UTF-8 is refused rather than sent back.
func validID(raw []byte) bool {
	switch raw[0] {
	case '"':
		return utf8.Valid(raw)
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	}
	return string(raw) == "null"
}

// trimSpace removes JSON whitespace from both ends of b.
func trimSpace(b []byte) []byte {
	for len(b) > 0 && isSpace(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && isSpace(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

func isSpace(ch byte) bool { return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' }

// reportPanic writes a recovered panic and its stack to stderr. It only ever runs on a bug.
func reportPanic(p any) {
	text := "mcp: panic: "
	switch v := p.(type) {
	case error:
		text += v.Error()
	case string:
		text += v
	default:
		text += "(non-error value)"
	}
	os.Stderr.WriteString(text + "\n")
	os.Stderr.Write(debug.Stack())
}
