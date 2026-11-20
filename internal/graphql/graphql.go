// Package graphql is the GraphQL v4 surface at POST /api/graphql. It resolves every field from the
// same store objects REST renders, and its mutations call the same store operations.
//
// The package is a complete small GraphQL server written against the standard library:
//
//	lexer.go, parser.go     the query language: tokens, the document syntax tree
//	schema.go               the type system registry, built from schema.graphql and
//	                        introspection.graphql by the same parser
//	validate.go             what can be decided from the schema alone; nothing invalid executes
//	coerce.go               argument literals and JSON variables to typed input values
//	exec.go                 the executor, which writes the response as it resolves
//	resolvers.go, mutations.go, search.go, connection.go, introspection.go
//	                        the resolvers, one per field of the schema
//	graphql.go              transport: authentication, single and batched requests
package graphql

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"sync"

	"forge/internal/httpd"
	"forge/internal/store"
)

//go:embed schema.graphql
var schemaSDL string

//go:embed introspection.graphql
var introspectionSDL string

// schema builds the registry once per process: it is immutable afterwards and shared by every API.
var schema = sync.OnceValues(func() (*Schema, error) {
	s, err := parseSchema(schemaSDL, introspectionSDL)
	if err != nil {
		return nil, errors.New("graphql schema: " + err.Message)
	}
	resolvers(s)
	introspection(s)
	s.link()
	return s, s.check()
})

const (
	// maxQueryBytes is the longest document accepted.
	maxQueryBytes = 1 << 20
	// maxPrepared is how many parsed and validated documents are kept for reuse, and
	// maxPreparedBytes how long a document may be to be kept.
	maxPrepared      = 256
	maxPreparedBytes = 16 << 10
)

// API serves /api/graphql.
type API struct {
	st     *store.Store
	schema *Schema
	// prepared holds the parse and validation result of recent documents by their text. Clients
	// send the same few documents with different variables, so most requests skip both steps.
	prepared map[string]*prepared
}

// prepared is a document that went through parsing and validation.
type prepared struct {
	doc  *Document
	errs []*Error // why it cannot be executed; nil when it can
}

// New returns the GraphQL surface over st. It panics if the schema definition is inconsistent,
// which is a build error of this package (the tests build the schema too).
func New(st *store.Store) *API {
	s, err := schema()
	if err != nil {
		panic(err)
	}
	return &API{st: st, schema: s, prepared: map[string]*prepared{}}
}

var (
	notFoundBody   = []byte(`{"message":"Not Found"}`)
	unauthorized   = []byte(`{"message":"This endpoint requires you to be authenticated.","documentation_url":"https://docs.github.com/graphql/guides/forming-calls-with-graphql#authenticating-with-graphql"}`)
	badCredentials = []byte(`{"message":"Bad credentials","documentation_url":"https://docs.github.com/graphql"}`)
	badJSON        = []byte(`{"message":"Problems parsing JSON","documentation_url":"https://docs.github.com/graphql"}`)
)

// Serve answers one request to /api/graphql. The store is locked by the caller. Authentication is
// decided before the body is looked at. A body that is a JSON object is one operation and gets one
// reply object; a JSON array is a batch and gets an array of replies in the same order. Everything
// GraphQL can say about a request (syntax, validation and execution errors) is a 200.
func (a *API) Serve(c *httpd.Ctx) {
	if c.Method != httpd.POST {
		c.JSON(404, notFoundBody)
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
	body := bytes.TrimLeft(c.Body, " \t\r\n\xEF\xBB\xBF")
	host := string(c.Host)
	out := c.Scratch[:0]
	switch {
	case len(body) > 0 && body[0] == '{':
		req, ok := decode(body)
		if !ok {
			c.JSON(400, badJSON)
			return
		}
		out = a.run(out, host, req)
	case len(body) > 0 && body[0] == '[':
		var batch []json.RawMessage
		if json.Unmarshal(body, &batch) != nil {
			c.JSON(400, badJSON)
			return
		}
		out = append(out, '[')
		for i, raw := range batch {
			if i > 0 {
				out = append(out, ',')
			}
			if req, ok := decode(raw); ok {
				out = a.run(out, host, req)
			} else {
				out = appendFailure(out, &Error{Message: "A batched operation must be a JSON object."})
			}
		}
		out = append(out, ']')
	default:
		c.JSON(400, badJSON)
		return
	}
	c.Scratch = out
	c.JSON(200, out)
}

// request is one operation as it arrives: a document, optionally the name of the operation to run
// in it, and the values of its variables.
type request struct {
	query     string
	hasQuery  bool
	operation string
	variables map[string]any
	invalid   string // why the request cannot run at all, when it cannot
}

// decode reads a request object. ok is false when raw is not a JSON object.
func decode(raw []byte) (req *request, ok bool) {
	var wire struct {
		Query         json.RawMessage `json:"query"`
		OperationName json.RawMessage `json:"operationName"`
		Variables     json.RawMessage `json:"variables"`
	}
	if raw = bytes.TrimSpace(raw); len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &wire) != nil {
		return nil, false
	}
	req = &request{}
	req.hasQuery = json.Unmarshal(wire.Query, &req.query) == nil && len(wire.Query) > 0 && wire.Query[0] == '"'
	if len(wire.OperationName) > 0 && wire.OperationName[0] == '"' {
		_ = json.Unmarshal(wire.OperationName, &req.operation)
	}
	vars := []byte(wire.Variables)
	if len(vars) > 0 && vars[0] == '"' {
		// Some clients send the variables as a string of JSON.
		var inner string
		_ = json.Unmarshal(vars, &inner)
		vars = bytes.TrimSpace([]byte(inner))
	}
	switch {
	case len(vars) == 0 || string(vars) == "null":
	case vars[0] == '{':
		// Numbers stay as written (json.Number), so an Int is never rounded through a float.
		dec := json.NewDecoder(bytes.NewReader(vars))
		dec.UseNumber()
		if dec.Decode(&req.variables) != nil {
			req.invalid = "The variables are not a valid JSON object."
		}
	default:
		req.invalid = "The variables must be a JSON object."
	}
	return req, true
}

// prepare parses and validates a document, or finds the result of having done so before.
func (a *API) prepare(src string) *prepared {
	if p := a.prepared[src]; p != nil {
		return p
	}
	p := &prepared{}
	doc, err := parseDocument(src)
	if err != nil {
		p.errs = []*Error{err}
	} else {
		p.doc, p.errs = doc, a.schema.validate(doc)
	}
	if len(src) <= maxPreparedBytes {
		if len(a.prepared) >= maxPrepared {
			clear(a.prepared)
		}
		a.prepared[src] = p
	}
	return p
}

// pick selects the operation to run: the named one, or the only one.
func (d *Document) pick(name string) (*Operation, *Error) {
	if name == "" {
		if len(d.Ops) == 1 {
			return d.Ops[0], nil
		}
		return nil, &Error{Message: "An operation name is required"}
	}
	for _, op := range d.Ops {
		if op.Name == name {
			return op, nil
		}
	}
	return nil, &Error{Message: `No operation named "` + name + `"`}
}

// run executes one request and appends its reply object to dst. It never fails: whatever goes
// wrong, a bug in a resolver included, becomes the reply's errors.
func (a *API) run(dst []byte, host string, req *request) (out []byte) {
	start := len(dst)
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		message := "Something went wrong while executing your query."
		if _, ok := r.(tooBig); ok {
			message = "The query asks for more than " + strconv.Itoa(maxFields) + " fields and list items."
		} else {
			fmt.Fprintf(os.Stderr, "graphql: panic: %v\n%s", r, debug.Stack())
		}
		out = appendFailure(dst[:start], &Error{Message: message})
	}()

	switch {
	case !req.hasQuery:
		return appendFailure(dst, &Error{Message: "A query attribute must be specified and must be a string."})
	case req.invalid != "":
		return appendFailure(dst, &Error{Message: req.invalid})
	case len(req.query) > maxQueryBytes:
		return appendFailure(dst, &Error{Message: "The query is longer than " + strconv.Itoa(maxQueryBytes) + " bytes."})
	}
	p := a.prepare(req.query)
	if p.errs != nil {
		return appendFailure(dst, p.errs...)
	}
	op, err := p.doc.pick(req.operation)
	if err != nil {
		return appendFailure(dst, err)
	}
	vars, errs := a.schema.variables(op, req.variables)
	if errs != nil {
		return appendFailure(dst, errs...)
	}

	q := &query{
		s: a.schema, st: a.st, host: host, doc: p.doc, op: op, vars: vars,
		budget: maxFields, groups: map[groupKey][]group{},
	}
	rootType := a.schema.Query
	if op.Kind == "mutation" {
		rootType = a.schema.Mutation
	}
	q.out = append(dst, `{"data":`...)
	data := len(q.out)
	if !q.object(rootType, root{}, nil) {
		q.out = append(q.out[:data], "null"...)
	}
	if len(q.errs) > 0 {
		q.out = appendErrors(append(q.out, `,"errors":`...), q.errs)
	}
	return append(q.out, '}')
}
