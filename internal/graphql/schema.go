package graphql

import (
	"errors"
	"reflect"
	"sort"
	"strings"
)

// The schema registry. It is built once from the type definitions in schema.graphql and
// introspection.graphql (parsed by this package's own parser) and the resolvers registered in
// resolvers.go and introspection.go. Validation, execution and introspection all read this one
// structure, so a field cannot be visible to one of them and missing from another: a field without
// a resolver, or a resolver without a field, fails the build of the schema.

// TypeKind is the kind of a named type.
type TypeKind uint8

const (
	Scalar TypeKind = iota
	Object
	Interface
	Union
	Enum
	InputObject
)

var kindNames = [...]string{"SCALAR", "OBJECT", "INTERFACE", "UNION", "ENUM", "INPUT_OBJECT"}

// Type is a named type of the schema.
type Type struct {
	Kind        TypeKind
	Name        string
	Description string
	Fields      []*FieldDef  // Object, Interface: sorted by name
	Interfaces  []*Type      // Object, Interface
	Possible    []*Type      // Interface, Union: the object types a value can have, sorted by name
	Values      []*EnumValue // Enum
	Inputs      []*InputDef  // InputObject

	ref        *TypeRef // the plain, nullable reference to this type
	fields     map[string]*FieldDef
	inputs     map[string]*InputDef
	values     map[string]*EnumValue
	possible   map[*Type]bool
	implements []string // interface names as written, until linked
	members    []string // union member names as written, until linked
}

func (t *Type) composite() bool { return t.Kind == Object || t.Kind == Interface || t.Kind == Union }
func (t *Type) abstract() bool  { return t.Kind == Interface || t.Kind == Union }
func (t *Type) isInput() bool   { return t.Kind == Scalar || t.Kind == Enum || t.Kind == InputObject }

// includes reports whether a value of object type o can appear where t is expected.
func (t *Type) includes(o *Type) bool { return t == o || t.possible[o] }

// overlaps reports whether some object type is both a t and a u.
func (t *Type) overlaps(u *Type) bool {
	if !t.abstract() {
		return u.includes(t)
	}
	for _, o := range t.Possible {
		if u.includes(o) {
			return true
		}
	}
	return false
}

// TypeRef is a type as used by a field, argument or variable: a named type or a list, each
// possibly non-null.
type TypeRef struct {
	Named   *Type    // set for a named type
	Elem    *TypeRef // set for a list
	NonNull bool
}

func (t *TypeRef) String() string {
	var s string
	if t.Elem != nil {
		s = "[" + t.Elem.String() + "]"
	} else {
		s = t.Named.Name
	}
	if t.NonNull {
		s += "!"
	}
	return s
}

// named returns the named type at the core of the reference.
func (t *TypeRef) named() *Type {
	for t.Elem != nil {
		t = t.Elem
	}
	return t.Named
}

// FieldDef is a field of an object or interface type.
type FieldDef struct {
	Name        string
	Description string
	Type        *TypeRef
	Args        []*InputDef // sorted by name
	Deprecated  bool
	Reason      string
	Owner       *Type

	resolve resolver
	expr    *TypeExpr
}

func (f *FieldDef) arg(name string) *InputDef {
	for _, a := range f.Args {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// InputDef is an argument of a field or directive, or a field of an input object.
type InputDef struct {
	Name        string
	Description string
	Type        *TypeRef
	Default     *Value // nil when there is no default

	value any // Default coerced to Type
	expr  *TypeExpr
}

// EnumValue is one value of an enum type.
type EnumValue struct {
	Name        string
	Description string
	Deprecated  bool
	Reason      string
}

// DirectiveDef is a directive the schema knows.
type DirectiveDef struct {
	Name        string
	Description string
	Args        []*InputDef
	Locations   []string
	Repeatable  bool
}

// resolver produces the value of a field of src. Arguments arrive coerced to their declared types.
type resolver func(q *query, src any, a args) (any, error)

// metaType is the pseudo-type in introspection.graphql that declares __schema and __type.
// Resolvers are registered on it before the schema is linked; linking dissolves it.
const metaType = "__Meta"

// Schema is the whole type system.
type Schema struct {
	Types      []*Type // sorted by name
	Directives []*DirectiveDef
	Query      *Type
	Mutation   *Type

	byName     map[string]*Type
	directives map[string]*DirectiveDef
	meta       map[string]*FieldDef   // __schema and __type, the fields of the query root no type lists
	typename   *FieldDef              // __typename, which the executor answers itself
	goTypes    map[reflect.Type]*Type // the object type a Go value of that type is; nil when ambiguous
	roots      map[string]string      // "query" -> "Query", as written in the schema definition
	problems   []string
}

func (s *Schema) problem(parts ...string) { s.problems = append(s.problems, strings.Join(parts, "")) }

// field returns the definition of the field called name on a value of type t: one of t's own
// fields, __typename on any composite type, or __schema and __type on the query root. It returns
// nil when there is no such field.
func (s *Schema) field(t *Type, name string) *FieldDef {
	if f := t.fields[name]; f != nil {
		return f
	}
	if name == "__typename" {
		return s.typename
	}
	if t == s.Query {
		return s.meta[name]
	}
	return nil
}

// typeOf returns the object type of a value a resolver produced for an interface or union, or nil
// when the value's Go type belongs to no single object type.
func (s *Schema) typeOf(v any) *Type { return s.goTypes[reflect.TypeOf(v)] }

// ---- type definitions (SDL) ----

// parseSchema parses type system documents into a schema that is not linked yet.
func parseSchema(sources ...string) (s *Schema, err *Error) {
	defer recoverError(&err)
	s = &Schema{
		byName:     map[string]*Type{},
		directives: map[string]*DirectiveDef{},
		meta:       map[string]*FieldDef{},
		goTypes:    map[reflect.Type]*Type{},
		roots:      map[string]string{},
	}
	for _, src := range sources {
		p := &parser{lex: newLexer(src)}
		p.advance()
		for p.tok.kind != tokEOF {
			p.typeSystemDefinition(s)
		}
	}
	return s, nil
}

// description consumes the optional description string in front of a definition.
func (p *parser) description() string {
	if p.tok.kind != tokString {
		return ""
	}
	s := p.tok.text
	p.advance()
	return s
}

func (s *Schema) declare(kind TypeKind, name, description string) *Type {
	if s.byName[name] != nil {
		s.problem("type ", name, " is defined twice")
	}
	t := &Type{Kind: kind, Name: name, Description: description}
	t.ref = &TypeRef{Named: t}
	s.byName[name] = t
	s.Types = append(s.Types, t)
	return t
}

func (p *parser) typeSystemDefinition(s *Schema) {
	description := p.description()
	keyword := p.name()
	switch keyword {
	case "schema":
		p.expect("{")
		for !p.skip("}") {
			role := p.name()
			p.expect(":")
			s.roots[role] = p.name()
		}
	case "scalar":
		s.declare(Scalar, p.name(), description)
	case "type", "interface":
		kind := Object
		if keyword == "interface" {
			kind = Interface
		}
		t := s.declare(kind, p.name(), description)
		t.fields = map[string]*FieldDef{}
		if p.isName("implements") {
			p.advance()
			p.skip("&")
			for {
				t.implements = append(t.implements, p.name())
				if !p.skip("&") {
					break
				}
			}
		}
		p.expect("{")
		for !p.skip("}") {
			f := p.fieldDefinition()
			f.Owner = t
			if t.fields[f.Name] != nil {
				s.problem("field ", t.Name, ".", f.Name, " is defined twice")
			}
			t.fields[f.Name] = f
			t.Fields = append(t.Fields, f)
		}
	case "union":
		t := s.declare(Union, p.name(), description)
		p.expect("=")
		p.skip("|")
		for {
			t.members = append(t.members, p.name())
			if !p.skip("|") {
				break
			}
		}
	case "enum":
		t := s.declare(Enum, p.name(), description)
		t.values = map[string]*EnumValue{}
		p.expect("{")
		for !p.skip("}") {
			v := &EnumValue{Description: p.description(), Name: p.name()}
			v.Deprecated, v.Reason = deprecation(p.directives(true))
			t.values[v.Name] = v
			t.Values = append(t.Values, v)
		}
	case "input":
		t := s.declare(InputObject, p.name(), description)
		t.inputs = map[string]*InputDef{}
		p.expect("{")
		for !p.skip("}") {
			in := p.inputValueDefinition()
			t.inputs[in.Name] = in
			t.Inputs = append(t.Inputs, in)
		}
	case "directive":
		d := &DirectiveDef{Description: description}
		p.expect("@")
		d.Name = p.name()
		if p.skip("(") {
			for !p.skip(")") {
				d.Args = append(d.Args, p.inputValueDefinition())
			}
		}
		if p.isName("repeatable") {
			p.advance()
			d.Repeatable = true
		}
		if !p.isName("on") {
			p.unexpected()
		}
		p.advance()
		p.skip("|")
		for {
			d.Locations = append(d.Locations, p.name())
			if !p.skip("|") {
				break
			}
		}
		s.directives[d.Name] = d
		s.Directives = append(s.Directives, d)
	default:
		panic(&Error{Message: "unknown type system definition: " + keyword})
	}
}

func (p *parser) fieldDefinition() *FieldDef {
	f := &FieldDef{Description: p.description(), Name: p.name()}
	if p.skip("(") {
		for !p.skip(")") {
			f.Args = append(f.Args, p.inputValueDefinition())
		}
	}
	p.expect(":")
	f.expr = p.typeExpr()
	f.Deprecated, f.Reason = deprecation(p.directives(true))
	return f
}

func (p *parser) inputValueDefinition() *InputDef {
	in := &InputDef{Description: p.description(), Name: p.name()}
	p.expect(":")
	in.expr = p.typeExpr()
	if p.skip("=") {
		in.Default = p.value(true)
	}
	p.directives(true)
	return in
}

// deprecation reads @deprecated(reason: "...") from the directives of a definition.
func deprecation(dirs []*Directive) (deprecated bool, reason string) {
	for _, d := range dirs {
		if d.Name != "deprecated" {
			continue
		}
		deprecated, reason = true, "No longer supported"
		for _, a := range d.Args {
			if a.Name == "reason" && a.Value.Kind == ValString {
				reason = a.Value.Text
			}
		}
	}
	return deprecated, reason
}

// ---- linking ----

// ref resolves a written type against the schema; it returns nil for an unknown type name.
func (s *Schema) ref(e *TypeExpr) *TypeRef {
	if e.Elem != nil {
		elem := s.ref(e.Elem)
		if elem == nil {
			return nil
		}
		return &TypeRef{Elem: elem, NonNull: e.NonNull}
	}
	t := s.byName[e.Name]
	if t == nil {
		return nil
	}
	return &TypeRef{Named: t, NonNull: e.NonNull}
}

func (s *Schema) linkInputs(owner string, inputs []*InputDef) {
	for _, in := range inputs {
		if in.Type = s.ref(in.expr); in.Type == nil {
			s.problem(owner, ".", in.Name, " has the unknown type ", in.expr.String())
			in.Type = &TypeRef{Named: s.byName["String"]}
		} else if !in.Type.named().isInput() {
			s.problem(owner, ".", in.Name, " is not of an input type")
		}
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Name < inputs[j].Name })
}

func (s *Schema) defaults(owner string, inputs []*InputDef) {
	for _, in := range inputs {
		if in.Default == nil {
			continue
		}
		v, _, ok := s.literal(in.Type, in.Default, nil, true)
		if !ok {
			s.problem("the default of ", owner, ".", in.Name, " does not fit its type")
		}
		in.value = v
	}
}

// link resolves every name in the parsed definitions, orders everything by name and separates the
// introspection meta-fields from the type that declares them.
func (s *Schema) link() {
	for _, t := range s.Types {
		for _, name := range t.implements {
			if i := s.byName[name]; i != nil && i.Kind == Interface {
				t.Interfaces = append(t.Interfaces, i)
				if t.Kind == Object {
					i.Possible = append(i.Possible, t)
				}
			} else {
				s.problem(t.Name, " implements ", name, ", which is not an interface")
			}
		}
		for _, name := range t.members {
			if m := s.byName[name]; m != nil && m.Kind == Object {
				t.Possible = append(t.Possible, m)
			} else {
				s.problem("union ", t.Name, " has the member ", name, ", which is not an object type")
			}
		}
		for _, f := range t.Fields {
			if f.Type = s.ref(f.expr); f.Type == nil {
				s.problem(t.Name, ".", f.Name, " has the unknown type ", f.expr.String())
				f.Type = &TypeRef{Named: s.byName["String"]}
			} else if f.Type.named().Kind == InputObject {
				s.problem(t.Name, ".", f.Name, " is of an input type")
			}
			s.linkInputs(t.Name+"."+f.Name, f.Args)
		}
		s.linkInputs(t.Name, t.Inputs)
	}
	for _, d := range s.Directives {
		s.linkInputs("@"+d.Name, d.Args)
	}
	for _, t := range s.Types {
		sort.Slice(t.Fields, func(i, j int) bool { return t.Fields[i].Name < t.Fields[j].Name })
		sort.Slice(t.Interfaces, func(i, j int) bool { return t.Interfaces[i].Name < t.Interfaces[j].Name })
		sort.Slice(t.Possible, func(i, j int) bool { return t.Possible[i].Name < t.Possible[j].Name })
		sort.Slice(t.Values, func(i, j int) bool { return t.Values[i].Name < t.Values[j].Name })
		t.possible = make(map[*Type]bool, len(t.Possible))
		for _, o := range t.Possible {
			t.possible[o] = true
		}
		// An object must have every field of the interfaces it implements.
		for _, i := range t.Interfaces {
			for _, f := range i.Fields {
				if t.fields[f.Name] == nil {
					s.problem(t.Name, " implements ", i.Name, " but has no field ", f.Name)
				}
			}
		}
	}
	// Defaults are coerced only now: an enum default needs its type linked.
	for _, t := range s.Types {
		for _, f := range t.Fields {
			s.defaults(t.Name+"."+f.Name, f.Args)
		}
		s.defaults(t.Name, t.Inputs)
	}
	for _, d := range s.Directives {
		s.defaults("@"+d.Name, d.Args)
	}
	sort.Slice(s.Types, func(i, j int) bool { return s.Types[i].Name < s.Types[j].Name })
	sort.Slice(s.Directives, func(i, j int) bool { return s.Directives[i].Name < s.Directives[j].Name })

	s.Query, s.Mutation = s.byName[s.roots["query"]], s.byName[s.roots["mutation"]]
	if s.Query == nil || s.Query.Kind != Object {
		s.problem("the schema has no query root type")
	}

	if str := s.byName["String"]; str != nil {
		s.typename = &FieldDef{Name: "__typename", Type: &TypeRef{Named: str, NonNull: true}}
	}

	// The meta-fields are declared on a pseudo-type so that they are parsed, typed and resolved like
	// every other field; from here on Schema.field finds them and no type lists them.
	if m := s.byName[metaType]; m != nil {
		for _, f := range m.Fields {
			s.meta[f.Name] = f
		}
		delete(s.byName, metaType)
		for i, t := range s.Types {
			if t == m {
				s.Types = append(s.Types[:i], s.Types[i+1:]...)
				break
			}
		}
	}
}

// ---- resolvers ----

// getters and callers are the two shapes a resolver is written in: a plain read of the parent
// value, or a call that takes arguments and may fail. T is the Go type of the parent value.
type (
	getters[T any] map[string]func(q *query, o T) any
	callers[T any] map[string]func(q *query, o T, a args) (any, error)
)

var errParent = errors.New("the parent value has the wrong Go type for this resolver")

// get registers argument-free resolvers for fields of the type called typeName.
func get[T any](s *Schema, typeName string, m getters[T]) {
	for name, fn := range m {
		s.bind(typeName, name, reflect.TypeFor[T](), func(q *query, src any, _ args) (any, error) {
			o, ok := src.(T)
			if !ok {
				return nil, errParent
			}
			return fn(q, o), nil
		})
	}
}

// call registers resolvers that take arguments or can fail.
func call[T any](s *Schema, typeName string, m callers[T]) {
	for name, fn := range m {
		s.bind(typeName, name, reflect.TypeFor[T](), func(q *query, src any, a args) (any, error) {
			o, ok := src.(T)
			if !ok {
				return nil, errParent
			}
			return fn(q, o, a)
		})
	}
}

func (s *Schema) bind(typeName, field string, parent reflect.Type, fn resolver) {
	t := s.byName[typeName]
	if t == nil {
		s.problem("resolver for ", typeName, ".", field, ", a type the schema does not define")
		return
	}
	f := t.fields[field]
	switch {
	case f == nil:
		s.problem("resolver for ", typeName, ".", field, ", which the schema does not define")
		return
	case f.resolve != nil:
		s.problem("two resolvers for ", typeName, ".", field)
	}
	f.resolve = fn
	if t.Kind != Object || typeName == metaType {
		return
	}
	// Remember which object type this Go type is, for values that arrive as an interface or union.
	if known, seen := s.goTypes[parent]; !seen {
		s.goTypes[parent] = t
	} else if known != t {
		s.goTypes[parent] = nil
	}
}

// check reports everything that is wrong with the finished schema: definition errors and object
// fields nobody resolves.
func (s *Schema) check() error {
	for _, t := range s.Types {
		if t.Kind != Object {
			continue
		}
		for _, f := range t.Fields {
			if f.resolve == nil {
				s.problem("no resolver for ", t.Name, ".", f.Name)
			}
		}
	}
	for name, f := range s.meta {
		if f.resolve == nil {
			s.problem("no resolver for the meta-field ", name)
		}
	}
	if len(s.problems) == 0 {
		return nil
	}
	sort.Strings(s.problems)
	return errors.New("graphql schema: " + strings.Join(s.problems, "; "))
}
