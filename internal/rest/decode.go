package rest

import (
	"encoding/json"
	"reflect"
	"unicode/utf8"
)

// Field types for request bodies where a plain Go type would lose what the client said.

// optString is a string field that tells "absent", "null" and a value apart: PATCH treats a missing
// key as "leave alone" and null as "clear".
type optString struct {
	Set   bool // the key was present
	Null  bool // and its value was null
	Value string
}

// UnmarshalJSON accepts a string or null; anything else is a type error (a 422 for the client).
func (o *optString) UnmarshalJSON(b []byte) error {
	*o = optString{Set: true}
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	if len(b) == 0 || b[0] != '"' {
		return &json.UnmarshalTypeError{Value: jsonKind(b), Type: reflect.TypeFor[string]()}
	}
	return json.Unmarshal(b, &o.Value)
}

// ptr returns the value for a patch: nil when the key was absent, "" for null.
func (o *optString) ptr() *string {
	if !o.Set {
		return nil
	}
	return &o.Value
}

// labelNames is the "labels" field of an issue: an array whose items are label names or objects
// with a "name", as the API accepts both.
type labelNames struct {
	Set   bool
	Names []string
}

// UnmarshalJSON accepts null (no labels) or an array of strings and {"name": ...} objects.
func (l *labelNames) UnmarshalJSON(b []byte) error {
	*l = labelNames{Set: true}
	if string(b) == "null" {
		return nil
	}
	var items []json.RawMessage
	if len(b) == 0 || b[0] != '[' || json.Unmarshal(b, &items) != nil {
		return &json.UnmarshalTypeError{Value: jsonKind(b), Type: reflect.TypeFor[[]string]()}
	}
	l.Names = make([]string, 0, len(items))
	for _, item := range items {
		var named struct {
			Name string `json:"name"`
		}
		switch {
		case len(item) > 0 && item[0] == '"':
			if err := json.Unmarshal(item, &named.Name); err != nil {
				return err
			}
		case len(item) > 0 && item[0] == '{':
			if err := json.Unmarshal(item, &named); err != nil {
				return err
			}
		default:
			return &json.UnmarshalTypeError{Value: jsonKind(item), Type: reflect.TypeFor[string]()}
		}
		l.Names = append(l.Names, named.Name)
	}
	return nil
}

// jsonKind names the kind of the JSON value b starts with, for type error messages.
func jsonKind(b []byte) string {
	if len(b) == 0 {
		return "value"
	}
	switch b[0] {
	case '"':
		return "string"
	case '{':
		return "object"
	case '[':
		return "array"
	case 't', 'f':
		return "bool"
	case 'n':
		return "null"
	}
	return "number"
}

// scanIssue reads the everyday body of an issue creation, {"title":"...","body":"..."}, straight
// out of the request buffer. It is a shortcut past encoding/json, not a second parser with its own
// opinions: it reports ok only for input that encoding/json decodes to exactly these two strings,
// and Decode handles everything else.
//
// Accepted: one object, JSON whitespace anywhere between tokens, the keys "title" and "body" in any
// order and any number of times (the last one wins, as in encoding/json), string values that are
// valid UTF-8 without escapes or control characters. Refused (ok false): any other key (encoding/json
// matches keys without case, and labels need resolving), a non-string value, a backslash, invalid
// UTF-8, and every syntax error.
//
// The returned slices are views into b.
func scanIssue(b []byte) (title, body []byte, ok bool) {
	i := scanSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return nil, nil, false
	}
	i = scanSpace(b, i+1)
	if i < len(b) && b[i] == '}' {
		return nil, nil, scanSpace(b, i+1) == len(b)
	}
	for {
		key, next, good := scanPlainString(b, i)
		if !good {
			return nil, nil, false
		}
		i = scanSpace(b, next)
		if i >= len(b) || b[i] != ':' {
			return nil, nil, false
		}
		value, next, good := scanPlainString(b, scanSpace(b, i+1))
		if !good {
			return nil, nil, false
		}
		switch string(key) {
		case "title":
			title = value
		case "body":
			body = value
		default:
			return nil, nil, false
		}
		i = scanSpace(b, next)
		if i >= len(b) {
			return nil, nil, false
		}
		if b[i] == '}' {
			return title, body, scanSpace(b, i+1) == len(b)
		}
		if b[i] != ',' {
			return nil, nil, false
		}
		i = scanSpace(b, i+1)
	}
}

// scanSpace returns the index of the first byte at or after i that is not JSON whitespace.
func scanSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	return i
}

// scanPlainString reads a JSON string starting at b[i] that needs no decoding: no escapes, no
// control characters, valid UTF-8. It returns the bytes between the quotes and the index after the
// closing quote.
func scanPlainString(b []byte, i int) (s []byte, next int, ok bool) {
	if i >= len(b) || b[i] != '"' {
		return nil, 0, false
	}
	start := i + 1
	ascii := true
	for j := start; j < len(b); j++ {
		switch plainByte[b[j]] {
		case byteASCII:
		case byteMulti:
			ascii = false
		case byteQuote:
			s = b[start:j]
			return s, j + 1, ascii || utf8.Valid(s)
		default:
			return nil, 0, false
		}
	}
	return nil, 0, false
}

// Classes of the bytes inside a JSON string, as scanPlainString sees them.
const (
	byteASCII = iota // stands for itself
	byteMulti        // part of a multi-byte UTF-8 sequence, to be validated
	byteQuote        // the closing quote
	byteStop         // a backslash or a control character: not a plain string
)

var plainByte = func() (t [256]uint8) {
	for ch := range t {
		switch {
		case ch == '"':
			t[ch] = byteQuote
		case ch == '\\' || ch < 0x20:
			t[ch] = byteStop
		case ch >= utf8.RuneSelf:
			t[ch] = byteMulti
		}
	}
	return t
}()
