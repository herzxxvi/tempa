package graphql

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"forge/internal/render"
)

// Input coercion: turning argument literals and JSON variable values into Go values of the
// declared input type. After coercion a value is one of
//
//	Int                       int
//	Float                     float64
//	String, ID, other scalars string
//	Boolean                   bool
//	enum                      string (the value's name)
//	list                      []any
//	input object              args
//	null                      nil

// args holds the arguments of a field, or the fields of an input object, by name. A name is
// absent when nothing was given and there is no default; it maps to nil for an explicit null.
type args map[string]any

// has reports whether name was given a value other than null.
func (a args) has(name string) bool { return a[name] != nil }

func (a args) str(name string) string {
	s, _ := a[name].(string)
	return s
}

func (a args) int(name string) (int, bool) {
	n, ok := a[name].(int)
	return n, ok
}

func (a args) flag(name string) bool {
	b, _ := a[name].(bool)
	return b
}

func (a args) list(name string) []any {
	l, _ := a[name].([]any)
	return l
}

// strings returns a list argument of strings or enum values.
func (a args) strings(name string) []string {
	l := a.list(name)
	out := make([]string, 0, len(l))
	for _, v := range l {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// object returns an input object argument; it is nil (and safe to read) when absent.
func (a args) object(name string) args {
	o, _ := a[name].(args)
	return o
}

// literal coerces a value written in a document to type t. vars are the operation's coerced
// variables. present is false when the value is a variable that was given no value; ok is false
// when the value does not fit the type. In static mode (validation, defaults) variables are not
// looked at: they are checked separately against the position's type.
func (s *Schema) literal(t *TypeRef, v *Value, vars map[string]any, static bool) (val any, present, ok bool) {
	switch v.Kind {
	case ValVariable:
		if static {
			return nil, false, true
		}
		val, present = vars[v.Text]
		return val, present, !(present && val == nil && t.NonNull)
	case ValNull:
		return nil, true, !t.NonNull
	}
	if t.Elem != nil {
		if v.Kind != ValList {
			// A single value where a list is expected is a list of that one value.
			e, _, ok := s.literal(t.Elem, v, vars, static)
			return []any{e}, true, ok
		}
		list := make([]any, len(v.List))
		for i, e := range v.List {
			x, present, ok := s.literal(t.Elem, e, vars, static)
			if !ok || (!present && !static && t.Elem.NonNull) {
				return nil, true, false
			}
			list[i] = x
		}
		return list, true, true
	}
	switch n := t.Named; n.Kind {
	case Scalar:
		val, ok = scalarLiteral(n.Name, v)
	case Enum:
		val, ok = v.Text, v.Kind == ValEnum && n.values[v.Text] != nil
	case InputObject:
		if v.Kind != ValObject {
			return nil, true, false
		}
		obj := make(args, len(n.Inputs))
		for _, in := range n.Inputs {
			var given *Argument
			for _, f := range v.Fields {
				if f.Name == in.Name {
					given = f
				}
			}
			if given != nil {
				x, present, ok := s.literal(in.Type, given.Value, vars, static)
				if !ok {
					return nil, true, false
				}
				if present {
					obj[in.Name] = x
					continue
				}
			}
			switch {
			case in.Default != nil:
				obj[in.Name] = in.value
			case in.Type.NonNull && !(static && given != nil):
				return nil, true, false
			}
		}
		val, ok = obj, true
	}
	return val, true, ok
}

func scalarLiteral(name string, v *Value) (any, bool) {
	switch name {
	case "Int":
		if v.Kind == ValInt {
			n, err := strconv.ParseInt(v.Text, 10, 32)
			return int(n), err == nil
		}
	case "Float":
		if v.Kind == ValInt || v.Kind == ValFloat {
			f, err := strconv.ParseFloat(v.Text, 64)
			return f, err == nil
		}
	case "Boolean":
		return v.Text == "true", v.Kind == ValBool
	case "ID":
		return v.Text, v.Kind == ValString || v.Kind == ValInt
	default: // String and the custom scalars, which are all strings on the wire
		return v.Text, v.Kind == ValString
	}
	return nil, false
}

// problem is one reason a variable's value does not fit its type.
type problem struct {
	path []any
	text string
}

// input coerces a decoded JSON value (a variable) to type t, collecting what does not fit. Unknown
// fields of an input object are ignored, so that a client written against a larger schema can
// still send its usual input.
func (s *Schema) input(t *TypeRef, v any, path []any, out *[]problem) any {
	bad := func(text string) any {
		*out = append(*out, problem{path: append([]any(nil), path...), text: text})
		return nil
	}
	if v == nil {
		if t.NonNull {
			return bad("Expected value to not be null")
		}
		return nil
	}
	if t.Elem != nil {
		list, ok := v.([]any)
		if !ok {
			list = []any{v}
		}
		res := make([]any, len(list))
		for i, e := range list {
			res[i] = s.input(t.Elem, e, append(path, i), out)
		}
		return res
	}
	switch n := t.Named; n.Kind {
	case Scalar:
		if val, ok := scalarInput(n.Name, v); ok {
			return val
		}
		return bad("Could not coerce value " + jsonText(v) + " to " + n.Name)
	case Enum:
		if name, ok := v.(string); ok && n.values[name] != nil {
			return name
		}
		names := make([]string, len(n.Values))
		for i, e := range n.Values {
			names[i] = e.Name
		}
		return bad("Expected " + jsonText(v) + " to be one of: " + joinNames(names))
	case InputObject:
		obj, ok := v.(map[string]any)
		if !ok {
			return bad("Expected " + jsonText(v) + " to be a key-value object")
		}
		res := make(args, len(n.Inputs))
		for _, in := range n.Inputs {
			given, present := obj[in.Name]
			switch {
			case present:
				res[in.Name] = s.input(in.Type, given, append(path, in.Name), out)
			case in.Default != nil:
				res[in.Name] = in.value
			case in.Type.NonNull:
				*out = append(*out, problem{path: append(append([]any(nil), path...), in.Name), text: "Expected value to not be null"})
			}
		}
		return res
	}
	return bad("Expected an input type")
}

func scalarInput(name string, v any) (any, bool) {
	switch name {
	case "Int":
		if f, ok := number(v); ok && f == math.Trunc(f) && f >= math.MinInt32 && f <= math.MaxInt32 {
			return int(f), true
		}
	case "Float":
		return number(v)
	case "Boolean":
		b, ok := v.(bool)
		return b, ok
	case "ID":
		switch x := v.(type) {
		case string:
			return x, true
		case json.Number:
			if _, err := strconv.ParseInt(string(x), 10, 64); err == nil {
				return string(x), true
			}
		}
	default:
		s, ok := v.(string)
		return s, ok
	}
	return nil, false
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	}
	return 0, false
}

// jsonText renders a decoded JSON value for an error message.
func jsonText(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return "the value"
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// variables coerces the request's variables to the types the operation declares. Any error means
// the operation must not run.
func (s *Schema) variables(op *Operation, raw map[string]any) (map[string]any, []*Error) {
	vars := make(map[string]any, len(op.Vars))
	var errs []*Error
	for _, vd := range op.Vars {
		given, present := raw[vd.Name]
		var problems []problem
		switch {
		case present:
			vars[vd.Name] = s.input(vd.ref, given, nil, &problems)
		case vd.Default != nil:
			vars[vd.Name], _, _ = s.literal(vd.ref, vd.Default, nil, true)
		case vd.ref.NonNull:
			problems = []problem{{text: "Expected value to not be null"}}
		}
		if len(problems) == 0 {
			continue
		}
		e := &Error{Message: "Variable $" + vd.Name + " of type " + vd.ref.String() + " was provided invalid value", Line: vd.Line, Col: vd.Col}
		ext := append([]byte(`{"value":`), jsonText(given)...)
		ext = append(ext, `,"problems":[`...)
		for i, p := range problems {
			if i > 0 {
				ext = append(ext, ',')
				e.Message += ","
			}
			ext = appendPath(append(ext, `{"path":`...), p.path)
			ext = render.AppendString(append(ext, `,"explanation":`...), p.text)
			ext = append(ext, '}')
			if len(p.path) > 0 {
				e.Message += " for " + pathText(p.path) + " (" + p.text + ")"
			}
		}
		e.Extensions = append(ext, "]}"...)
		errs = append(errs, e)
	}
	return vars, errs
}

// pathText renders a path inside a variable as "input.labels.0".
func pathText(path []any) string {
	parts := make([]string, len(path))
	for i, p := range path {
		switch p := p.(type) {
		case string:
			parts[i] = p
		case int:
			parts[i] = strconv.Itoa(p)
		}
	}
	return strings.Join(parts, ".")
}
