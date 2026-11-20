package graphql

import (
	"math"
	"reflect"
	"strconv"

	"forge/internal/render"
	"forge/internal/store"
)

// maxFields is how many fields and list items one operation may produce before it is cut off,
// which bounds the work and the size of a response whatever the document asks for.
const maxFields = 500000

// tooBig is what the executor panics with when an operation exceeds maxFields; API.run turns it
// into the operation's error reply.
type tooBig struct{}

// query is the execution of one operation. The response is written straight into out, in
// selection order; when a non-null position turns out to have no value, out is cut back to the
// nearest nullable position, which becomes null.
type query struct {
	s    *Schema
	st   *store.Store
	host string // Host of the request, for absolute URLs
	doc  *Document
	op   *Operation
	vars map[string]any

	out    []byte
	errs   []*Error
	path   []any
	budget int
	groups map[groupKey][]group
}

// group is the set of field nodes that produce one response key of an object.
type group struct {
	key    string
	fields []*Field
}

// groupKey names the selection of an object: the field group it is the value of (nil for the
// operation's root) and its runtime type.
type groupKey struct {
	parent *group
	t      *Type
}

func (q *query) spend(n int) {
	if q.budget -= n; q.budget < 0 {
		panic(tooBig{})
	}
}

// fail records an error for the field being executed.
func (q *query) fail(g *group, e *Error) {
	if e.Path == nil {
		e.Path = append([]any(nil), q.path...)
	}
	if e.Line == 0 && g != nil {
		e.Line, e.Col = g.fields[0].Line, g.fields[0].Col
	}
	q.errs = append(q.errs, e)
}

// failAt records an error for one element of the list the current field returns.
func (q *query) failAt(index int, e *Error) {
	e.Path = append(append([]any(nil), q.path...), index)
	q.errs = append(q.errs, e)
}

// included applies @skip and @include.
func (q *query) included(dirs []*Directive) bool {
	for _, d := range dirs {
		if d.Name != "skip" && d.Name != "include" {
			continue
		}
		on := false
		for _, a := range d.Args {
			if a.Name == "if" {
				v, _, _ := q.s.literal(q.s.directives[d.Name].Args[0].Type, a.Value, q.vars, false)
				on, _ = v.(bool)
			}
		}
		if on == (d.Name == "skip") {
			return false
		}
	}
	return true
}

// applies reports whether a fragment with the given type condition selects on an object of type t.
func (q *query) applies(condition string, t *Type) bool {
	c := q.s.byName[condition]
	return c != nil && c.includes(t)
}

// collect gathers the fields a selection set selects on an object of type t into groups by
// response key, in the order the keys first appear, looking through fragments.
func (q *query) collect(t *Type, sels []Selection, groups []group, seen map[string]bool) []group {
	for _, sel := range sels {
		q.spend(1)
		switch n := sel.(type) {
		case *Field:
			if !q.included(n.Dirs) {
				continue
			}
			key := n.Key()
			i := 0
			for i < len(groups) && groups[i].key != key {
				i++
			}
			if i == len(groups) {
				groups = append(groups, group{key: key})
			}
			groups[i].fields = append(groups[i].fields, n)
		case *InlineFragment:
			if q.included(n.Dirs) && (n.On == "" || q.applies(n.On, t)) {
				groups = q.collect(t, n.Sel, groups, seen)
			}
		case *FragmentSpread:
			if !q.included(n.Dirs) || seen[n.Name] {
				continue
			}
			seen[n.Name] = true
			if f := q.doc.frag[n.Name]; f != nil && q.applies(f.On, t) {
				groups = q.collect(t, f.Sel, groups, seen)
			}
		}
	}
	return groups
}

// selection returns the field groups selected on an object of type t that is the value of parent
// (nil: the root). The result is computed once per parent and type, so a list of a hundred objects
// collects its fields once.
func (q *query) selection(parent *group, t *Type) []group {
	key := groupKey{parent, t}
	if groups, ok := q.groups[key]; ok {
		return groups
	}
	var groups []group
	seen := map[string]bool{}
	if parent == nil {
		groups = q.collect(t, q.op.Sel, groups, seen)
	} else {
		for _, f := range parent.fields {
			groups = q.collect(t, f.Sel, groups, seen)
		}
	}
	q.groups[key] = groups
	return groups
}

// object writes src, an object of type t, with the fields selected below parent. It reports false
// when one of its non-null fields has no value: the object itself then has none.
func (q *query) object(t *Type, src any, parent *group) bool {
	groups := q.selection(parent, t)
	q.out = append(q.out, '{')
	for i := range groups {
		g := &groups[i]
		if i > 0 {
			q.out = append(q.out, ',')
		}
		q.out = render.AppendString(q.out, g.key)
		q.out = append(q.out, ':')
		q.path = append(q.path, g.key)
		ok := q.field(t, src, g)
		q.path = q.path[:len(q.path)-1]
		if !ok {
			return false
		}
	}
	q.out = append(q.out, '}')
	return true
}

// field resolves one field of src and writes its value.
func (q *query) field(t *Type, src any, g *group) bool {
	q.spend(1)
	f := g.fields[0]
	def := q.s.field(t, f.Name)
	if def == q.s.typename {
		q.out = render.AppendString(q.out, t.Name)
		return true
	}
	if def == nil { // validation makes this unreachable
		q.fail(g, &Error{Type: "INTERNAL", Message: "Field '" + f.Name + "' doesn't exist on type '" + t.Name + "'"})
		q.out = append(q.out, "null"...)
		return true
	}
	var v any
	a, e := q.arguments(def, f)
	if e == nil {
		var err error
		if v, err = def.resolve(q, src, a); err != nil {
			e, v = failure(err), nil
		}
	}
	if e != nil {
		q.fail(g, e)
	}
	return q.value(def.Type, v, def, g, e != nil)
}

// arguments coerces the arguments of a field node, filling in the defaults of those not given.
func (q *query) arguments(def *FieldDef, f *Field) (args, *Error) {
	if len(def.Args) == 0 {
		return nil, nil
	}
	a := make(args, len(def.Args))
	for _, d := range def.Args {
		var given *Argument
		for _, x := range f.Args {
			if x.Name == d.Name {
				given = x
			}
		}
		if given != nil {
			v, present, ok := q.s.literal(d.Type, given.Value, q.vars, false)
			if !ok {
				return nil, &Error{Message: "Argument '" + d.Name + "' on Field '" + f.Name + "' has an invalid value (" + given.Value.String() + "). Expected type '" + d.Type.String() + "'."}
			}
			if present {
				a[d.Name] = v
				continue
			}
		}
		switch {
		case d.Default != nil:
			a[d.Name] = d.value
		case d.Type.NonNull:
			return nil, &Error{Message: "Field '" + f.Name + "' is missing required arguments: " + d.Name}
		}
	}
	return a, nil
}

// isNil reports whether a resolver's result stands for null: nil itself or a nil pointer.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// value writes v at a position of type t. When v or something non-null below it has no value, a
// nullable position becomes null and a non-null one reports false, passing the problem upwards.
// quiet says the missing value is already explained by a recorded error.
func (q *query) value(t *TypeRef, v any, def *FieldDef, g *group, quiet bool) bool {
	start := len(q.out)
	if q.write(t, v, def, g, quiet) {
		return true
	}
	q.out = q.out[:start]
	if t.NonNull {
		return false
	}
	q.out = append(q.out, "null"...)
	return true
}

func (q *query) write(t *TypeRef, v any, def *FieldDef, g *group, quiet bool) bool {
	if isNil(v) {
		if t.NonNull && !quiet {
			q.fail(g, &Error{Type: "INTERNAL", Message: "Cannot return null for non-nullable field " + def.Owner.Name + "." + def.Name})
		}
		return false
	}
	internal := func(what string) bool {
		q.fail(g, &Error{Type: "INTERNAL", Message: "The resolver of " + def.Owner.Name + "." + def.Name + " returned " + what})
		return false
	}
	if t.Elem != nil {
		list, ok := v.([]any)
		if !ok {
			return internal("a value that is not a list")
		}
		q.spend(len(list))
		q.out = append(q.out, '[')
		for i, e := range list {
			if i > 0 {
				q.out = append(q.out, ',')
			}
			q.path = append(q.path, i)
			ok := q.value(t.Elem, e, def, g, false)
			q.path = q.path[:len(q.path)-1]
			if !ok {
				return false
			}
		}
		q.out = append(q.out, ']')
		return true
	}
	switch n := t.Named; n.Kind {
	case Object:
		return q.object(n, v, g)
	case Interface, Union:
		o := q.s.typeOf(v)
		if o == nil || !n.includes(o) {
			return internal("a value that is no " + n.Name)
		}
		return q.object(o, v, g)
	default:
		if !q.leaf(n, v) {
			return internal("a value that is no " + n.Name)
		}
		return true
	}
}

// leaf writes a scalar or enum value; false when v is not a value of the type.
func (q *query) leaf(t *Type, v any) bool {
	if t.Kind == Enum {
		name, ok := v.(string)
		if !ok || t.values[name] == nil {
			return false
		}
		q.out = render.AppendString(q.out, name)
		return true
	}
	switch t.Name {
	case "Int":
		var n int64
		switch x := v.(type) {
		case int:
			n = int64(x)
		case int64:
			n = x
		default:
			return false
		}
		if n < math.MinInt32 || n > math.MaxInt32 {
			return false
		}
		q.out = strconv.AppendInt(q.out, n, 10)
	case "Float":
		var f float64
		switch x := v.(type) {
		case float64:
			f = x
		case int:
			f = float64(x)
		default:
			return false
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return false
		}
		q.out = strconv.AppendFloat(q.out, f, 'g', -1, 64)
	case "Boolean":
		b, ok := v.(bool)
		if !ok {
			return false
		}
		q.out = strconv.AppendBool(q.out, b)
	default: // String, ID and the custom scalars
		s, ok := v.(string)
		if !ok {
			return false
		}
		q.out = render.AppendString(q.out, s)
	}
	return true
}

// items turns a typed slice into the []any the executor takes for a list.
func items[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
