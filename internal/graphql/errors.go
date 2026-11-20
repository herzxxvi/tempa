package graphql

import (
	"strconv"

	"forge/internal/render"
	"forge/internal/store"
)

// Error is one entry of a response's "errors" array.
type Error struct {
	Message    string
	Type       string // GitHub's class of an execution error: NOT_FOUND, UNPROCESSABLE, ...
	Path       []any  // response keys (string) and list indexes (int) leading to the failed field
	Line       int    // 1-based position in the document; 0 when there is none
	Col        int
	Extensions []byte // a rendered JSON object, or nil
}

func (e *Error) Error() string { return e.Message }

// append renders the entry in GitHub's key order.
func (e *Error) append(dst []byte) []byte {
	dst = append(dst, '{')
	if e.Type != "" {
		dst = append(dst, `"type":`...)
		dst = append(render.AppendString(dst, e.Type), ',')
	}
	if e.Path != nil {
		dst = appendPath(append(dst, `"path":`...), e.Path)
		dst = append(dst, ',')
	}
	if e.Extensions != nil {
		dst = append(dst, `"extensions":`...)
		dst = append(append(dst, e.Extensions...), ',')
	}
	if e.Line > 0 {
		dst = append(dst, `"locations":[{"line":`...)
		dst = strconv.AppendInt(dst, int64(e.Line), 10)
		dst = append(dst, `,"column":`...)
		dst = strconv.AppendInt(dst, int64(e.Col), 10)
		dst = append(dst, "}],"...)
	}
	dst = append(dst, `"message":`...)
	dst = render.AppendString(dst, e.Message)
	return append(dst, '}')
}

// appendPath renders a path of response keys and list indexes as a JSON array.
func appendPath(dst []byte, path []any) []byte {
	dst = append(dst, '[')
	for i, p := range path {
		if i > 0 {
			dst = append(dst, ',')
		}
		switch p := p.(type) {
		case string:
			dst = render.AppendString(dst, p)
		case int:
			dst = strconv.AppendInt(dst, int64(p), 10)
		}
	}
	return append(dst, ']')
}

// appendErrors renders a non-empty "errors" array.
func appendErrors(dst []byte, errs []*Error) []byte {
	dst = append(dst, '[')
	for i, e := range errs {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = e.append(dst)
	}
	return append(dst, ']')
}

// appendFailure renders the whole reply of an operation that did not execute: errors and no data.
func appendFailure(dst []byte, errs ...*Error) []byte {
	dst = append(dst, `{"errors":`...)
	dst = appendErrors(dst, errs)
	return append(dst, '}')
}

// extensions renders {"code": code, key: value, ...} from pairs of strings.
func extensions(code string, pairs ...string) []byte {
	dst := render.AppendString([]byte(`{"code":`), code)
	for i := 0; i+1 < len(pairs); i += 2 {
		dst = append(dst, ',')
		dst = render.AppendString(dst, pairs[i])
		dst = append(dst, ':')
		dst = render.AppendString(dst, pairs[i+1])
	}
	return append(dst, '}')
}

// notFound is the error for a global id that names nothing the viewer can see.
func notFound(id string) *Error {
	return &Error{Type: "NOT_FOUND", Message: "Could not resolve to a node with the global id of '" + id + "'"}
}

// failure turns whatever a resolver returned as its error into a response error. A store error
// keeps the store's message and maps its REST status to GitHub's GraphQL error type.
func failure(err error) *Error {
	switch e := err.(type) {
	case *Error:
		clone := *e
		return &clone
	case *store.Error:
		kind := "UNPROCESSABLE"
		switch e.Status {
		case 404:
			kind = "NOT_FOUND"
		case 403:
			kind = "FORBIDDEN"
		}
		return &Error{Type: kind, Message: e.Error()}
	}
	return &Error{Type: "INTERNAL", Message: err.Error()}
}
