package graphql

import (
	"sort"
	"strconv"
	"strings"
)

// Validation: everything that can be decided about a document from the schema alone. A document
// with a validation error is never executed, so no mutation of an invalid document runs.

const (
	// maxDepth is how many fields deep an operation may select, fragments included.
	maxDepth = 200
	// maxMergeWork bounds the field-merge check, whose cost grows with every place a fragment is
	// spread.
	maxMergeWork = 200000
)

type validator struct {
	s    *Schema
	doc  *Document
	errs []*Error
	path []string // where the walk is, as GitHub reports it: "query Name", field keys, "... on T"

	// While an operation is walked:
	op       *Operation
	declared map[string]*VarDef
	used     map[string]bool // variables the operation uses, directly or through fragments
	entered  map[string]bool // fragments already walked for this operation
	// quiet is positive inside a fragment reached from an operation. The fragment's own errors were
	// reported when it was checked as a definition; only the operation's variables matter here.
	quiet int

	spread map[string]bool // fragments spread anywhere in the document
	work   int
}

// validate returns the validation errors of doc.
func (s *Schema) validate(doc *Document) []*Error {
	v := &validator{s: s, doc: doc, spread: map[string]bool{}}
	v.names()
	if !v.shape() {
		return v.errs
	}
	for _, f := range doc.Frags {
		v.fragment(f)
	}
	for _, op := range doc.Ops {
		v.operation(op)
	}
	v.op, v.path = nil, nil
	for _, f := range doc.Frags {
		if !v.spread[f.Name] {
			v.path = append(v.path[:0], "fragment "+f.Name)
			v.add(f.Line, f.Col, "useAndDefineFragment", "Fragment "+f.Name+" was defined, but not used", "fragmentName", f.Name)
		}
	}
	if len(v.errs) == 0 {
		for _, op := range doc.Ops {
			root := s.Query
			if op.Kind == "mutation" {
				root = s.Mutation
			}
			v.path = append(v.path[:0], op.label())
			v.merge([]scope{{root, op.Sel}})
		}
	}
	return v.errs
}

// add records an error at the current path.
func (v *validator) add(line, col int, code, message string, pairs ...string) {
	e := &Error{Message: message, Line: line, Col: col, Path: make([]any, len(v.path)), Extensions: extensions(code, pairs...)}
	for i, p := range v.path {
		e.Path[i] = p
	}
	v.errs = append(v.errs, e)
}

// schema records an error about the document's use of the schema, unless the walk is inside a
// fragment whose errors were already reported.
func (v *validator) schema(line, col int, code, message string, pairs ...string) {
	if v.quiet == 0 {
		v.add(line, col, code, message, pairs...)
	}
}

func (v *validator) push(s string) { v.path = append(v.path, s) }
func (v *validator) pop()          { v.path = v.path[:len(v.path)-1] }

// names checks that operations and fragments can be told apart.
func (v *validator) names() {
	ops := map[string]bool{}
	for _, op := range v.doc.Ops {
		v.path = append(v.path[:0], op.label())
		switch {
		case op.Name == "":
			if len(v.doc.Ops) > 1 {
				v.add(op.Line, op.Col, "uniquelyNamedOperations", "Operation name is required when multiple operations are present")
			}
		case ops[op.Name]:
			v.add(op.Line, op.Col, "uniquelyNamedOperations", `Operation name "`+op.Name+`" must be unique`, "operationName", op.Name)
		}
		ops[op.Name] = true
	}
	frags := map[string]bool{}
	for _, f := range v.doc.Frags {
		v.path = append(v.path[:0], "fragment "+f.Name)
		if frags[f.Name] {
			v.add(f.Line, f.Col, "fragmentNotUnique", `Fragment name "`+f.Name+`" must be unique`, "fragmentName", f.Name)
		}
		frags[f.Name] = true
	}
}

// shape rejects fragment cycles and operations that select too deep. Everything after it walks
// through fragment spreads and relies on both.
func (v *validator) shape() bool {
	const visiting = -1
	depths := map[string]int{}
	cyclic := false
	var depth func(sels []Selection) int
	depth = func(sels []Selection) int {
		deepest := 0
		for _, sel := range sels {
			d := 0
			switch n := sel.(type) {
			case *Field:
				d = 1 + depth(n.Sel)
			case *InlineFragment:
				d = depth(n.Sel)
			case *FragmentSpread:
				f := v.doc.frag[n.Name]
				if f == nil {
					continue
				}
				known, seen := depths[n.Name]
				switch {
				case !seen:
					depths[n.Name] = visiting
					known = depth(f.Sel)
					depths[n.Name] = known
				case known == visiting:
					cyclic = true
					v.path = append(v.path[:0], "fragment "+f.Name)
					v.add(f.Line, f.Col, "infiniteLoop", "Fragment "+f.Name+" contains an infinite loop", "fragmentName", f.Name)
					known = 0
				}
				d = known
			}
			deepest = max(deepest, d)
		}
		return deepest
	}
	for _, f := range v.doc.Frags {
		if _, seen := depths[f.Name]; !seen {
			depths[f.Name] = visiting
			depths[f.Name] = depth(f.Sel)
		}
	}
	if cyclic {
		return false
	}
	ok := true
	for _, op := range v.doc.Ops {
		if d := depth(op.Sel); d > maxDepth {
			v.path = append(v.path[:0], op.label())
			v.add(op.Line, op.Col, "maxDepthExceeded", "Query has depth of "+strconv.Itoa(d)+", which exceeds max depth of "+strconv.Itoa(maxDepth))
			ok = false
		}
	}
	return ok
}

// condition resolves the type condition of a fragment; nil (after reporting) when it names no
// composite type.
func (v *validator) condition(name string, line, col int) *Type {
	t := v.s.byName[name]
	switch {
	case t == nil:
		v.schema(line, col, "undefinedType", "No such type "+name+", so it can't be a fragment condition", "typeName", name)
		return nil
	case !t.composite():
		v.schema(line, col, "fragmentOnNonCompositeType", "Invalid fragment on type "+name+" (must be Union, Interface, or Object)", "typeName", name)
		return nil
	}
	return t
}

// fragment checks a fragment definition on its own.
func (v *validator) fragment(f *Fragment) {
	v.op, v.quiet = nil, 0
	v.path = append(v.path[:0], "fragment "+f.Name)
	t := v.condition(f.On, f.Line, f.Col)
	v.directives(f.Dirs)
	if t != nil {
		v.selections(t, f.Sel)
	}
}

func (op *Operation) title() string {
	if op.Name == "" {
		return "anonymous " + op.Kind
	}
	return op.Name
}

// operation checks one operation: its variables, its selections and, through the fragments it
// spreads, every use of its variables.
func (v *validator) operation(op *Operation) {
	v.op, v.quiet = op, 0
	v.declared, v.used, v.entered = map[string]*VarDef{}, map[string]bool{}, map[string]bool{}
	v.path = append(v.path[:0], op.label())
	var root *Type
	switch op.Kind {
	case "query":
		root = v.s.Query
	case "mutation":
		root = v.s.Mutation
	}
	if root == nil {
		v.add(op.Line, op.Col, "missingSubscriptionConfiguration", "Schema is not configured for "+op.Kind+"s")
		return
	}
	for _, vd := range op.Vars {
		v.variable(vd)
	}
	v.directives(op.Dirs)
	v.selections(root, op.Sel)
	for _, vd := range op.Vars {
		if !v.used[vd.Name] {
			v.add(vd.Line, vd.Col, "variableNotUsed", "Variable $"+vd.Name+" is declared by "+op.title()+" but not used", "variableName", vd.Name)
		}
	}
}

func (v *validator) variable(vd *VarDef) {
	if v.declared[vd.Name] != nil {
		v.add(vd.Line, vd.Col, "variableNotUnique", `There can only be one variable named "`+vd.Name+`"`, "variableName", vd.Name)
		return
	}
	v.declared[vd.Name] = vd
	inner := vd.Type
	for inner.Elem != nil {
		inner = inner.Elem
	}
	vd.ref = v.s.ref(vd.Type)
	switch {
	case vd.ref == nil:
		v.add(vd.Line, vd.Col, "variableRequiresValidType", inner.Name+" isn't a defined input type (on $"+vd.Name+")", "typeName", inner.Name, "variableName", vd.Name)
	case !vd.ref.named().isInput():
		vd.ref = nil
		v.add(vd.Line, vd.Col, "variableRequiresValidType", inner.Name+" isn't a valid input type (on $"+vd.Name+")", "typeName", inner.Name, "variableName", vd.Name)
	case vd.Default != nil:
		if _, _, ok := v.s.literal(vd.ref, vd.Default, nil, true); !ok {
			v.add(vd.Line, vd.Col, "defaultValueInvalidType", "Default value for $"+vd.Name+" doesn't match type "+vd.Type.String(), "variableName", vd.Name)
		}
	}
}

func (v *validator) selections(parent *Type, sels []Selection) {
	for _, sel := range sels {
		switch n := sel.(type) {
		case *Field:
			v.push(n.Key())
			v.field(parent, n)
			v.pop()
		case *InlineFragment:
			t := parent
			label := "..."
			if n.On != "" {
				label = "... on " + n.On
				if t = v.condition(n.On, n.Line, n.Col); t != nil && !parent.overlaps(t) {
					v.schema(n.Line, n.Col, "cannotSpreadFragment", "Fragment on "+t.Name+" can't be spread inside "+parent.Name, "typeName", t.Name, "parentName", parent.Name)
				}
			}
			v.push(label)
			v.directives(n.Dirs)
			if t != nil {
				v.selections(t, n.Sel)
			}
			v.pop()
		case *FragmentSpread:
			v.push("... " + n.Name)
			v.spreadOf(parent, n)
			v.pop()
		}
	}
}

func (v *validator) spreadOf(parent *Type, n *FragmentSpread) {
	v.directives(n.Dirs)
	f := v.doc.frag[n.Name]
	if f == nil {
		v.schema(n.Line, n.Col, "undefinedFragment", "Fragment "+n.Name+" was used, but not defined", "fragmentName", n.Name)
		return
	}
	v.spread[n.Name] = true
	t := v.s.byName[f.On]
	if t == nil || !t.composite() {
		return // reported with the fragment's definition
	}
	if !parent.overlaps(t) {
		v.schema(n.Line, n.Col, "cannotSpreadFragment", "Fragment "+n.Name+" on "+t.Name+" can't be spread inside "+parent.Name, "typeName", t.Name, "fragmentName", n.Name, "parentName", parent.Name)
	}
	if v.op == nil || v.entered[n.Name] {
		return
	}
	// First time this operation reaches the fragment: walk it for the operation's variables.
	v.entered[n.Name] = true
	v.quiet++
	v.directives(f.Dirs)
	v.selections(t, f.Sel)
	v.quiet--
}

func (v *validator) field(parent *Type, f *Field) {
	def := v.s.field(parent, f.Name)
	if def == nil {
		v.schema(f.Line, f.Col, "undefinedField", "Field '"+f.Name+"' doesn't exist on type '"+parent.Name+"'", "typeName", parent.Name, "fieldName", f.Name)
		return
	}
	v.arguments(def.Args, f.Args, "Field", f.Name, f.Line, f.Col)
	v.directives(f.Dirs)
	t := def.Type.named()
	switch {
	case t.composite() && len(f.Sel) == 0:
		v.schema(f.Line, f.Col, "selectionMismatch", "Field must have selections (field '"+f.Name+"' returns "+t.Name+" but has no selections. Did you mean '"+f.Name+" { ... }'?)", "nodeName", "field '"+f.Name+"'", "typeName", t.Name)
	case t.composite():
		v.selections(t, f.Sel)
	case len(f.Sel) > 0:
		var names []string
		for _, sel := range f.Sel {
			switch n := sel.(type) {
			case *Field:
				names = append(names, n.Name)
			case *InlineFragment:
				names = append(names, "... on "+n.On)
			case *FragmentSpread:
				names = append(names, "..."+n.Name)
			}
		}
		kind := "scalars"
		if t.Kind == Enum {
			kind = "enums"
		}
		v.schema(f.Line, f.Col, "selectionMismatch", "Selections can't be made on "+kind+" (field '"+f.Name+"' returns "+t.Name+" but has selections ["+joinNames(names)+"])", "nodeName", "field '"+f.Name+"'", "typeName", t.Name)
	}
}

func (v *validator) directives(dirs []*Directive) {
	for _, d := range dirs {
		def := v.s.directives[d.Name]
		if def == nil || (d.Name != "skip" && d.Name != "include") {
			// A directive this server does not act on is ignored, but what it uses is still used.
			for _, a := range d.Args {
				v.usage(nil, false, a.Value, a.Name)
			}
			continue
		}
		v.arguments(def.Args, d.Args, "Directive", d.Name, d.Line, d.Col)
	}
}

// arguments checks the arguments given to a field or directive against its definition.
func (v *validator) arguments(defs []*InputDef, given []*Argument, kind, owner string, line, col int) {
next:
	for i, a := range given {
		for _, b := range given[:i] {
			if b.Name == a.Name {
				v.schema(a.Line, a.Col, "argumentNotUnique", `There can be only one argument named "`+a.Name+`"`, "name", a.Name)
				continue next
			}
		}
		var def *InputDef
		for _, d := range defs {
			if d.Name == a.Name {
				def = d
			}
		}
		if def == nil {
			v.schema(a.Line, a.Col, "argumentNotAccepted", kind+" '"+owner+"' doesn't accept argument '"+a.Name+"'", "name", owner, "typeName", kind, "argumentName", a.Name)
			v.usage(nil, false, a.Value, a.Name)
			continue
		}
		v.usage(def.Type, def.Default != nil, a.Value, a.Name)
		if _, _, ok := v.s.literal(def.Type, a.Value, nil, true); !ok {
			v.schema(a.Line, a.Col, "argumentLiteralsIncompatible", "Argument '"+a.Name+"' on "+kind+" '"+owner+"' has an invalid value ("+a.Value.String()+"). Expected type '"+def.Type.String()+"'.", "typeName", kind, "argumentName", a.Name)
		}
	}
	var missing []string
	for _, d := range defs {
		if !d.Type.NonNull || d.Default != nil {
			continue
		}
		found := false
		for _, a := range given {
			found = found || a.Name == d.Name
		}
		if !found {
			missing = append(missing, d.Name)
		}
	}
	if len(missing) > 0 {
		v.schema(line, col, "missingRequiredArguments", kind+" '"+owner+"' is missing required arguments: "+joinNames(missing), "className", kind, "name", owner, "arguments", joinNames(missing))
	}
}

// usage finds the variables inside a value and checks each against the type its position expects.
// t is nil when the position has no known type.
func (v *validator) usage(t *TypeRef, hasDefault bool, val *Value, position string) {
	// A single value in a list position stands for a list of one: look at the element type.
	for t != nil && t.Elem != nil && val.Kind != ValList && val.Kind != ValVariable {
		t, hasDefault = t.Elem, false
	}
	switch val.Kind {
	case ValVariable:
		v.variableUse(val, t, hasDefault, position)
	case ValList:
		var elem *TypeRef
		if t != nil {
			elem = t.Elem
		}
		for _, e := range val.List {
			v.usage(elem, false, e, position)
		}
	case ValObject:
		for _, f := range val.Fields {
			var ft *TypeRef
			def := false
			if t != nil && t.Elem == nil && t.Named.Kind == InputObject {
				if in := t.Named.inputs[f.Name]; in != nil {
					ft, def = in.Type, in.Default != nil
				}
			}
			v.usage(ft, def, f.Value, f.Name)
		}
	}
}

func (v *validator) variableUse(val *Value, t *TypeRef, hasDefault bool, position string) {
	if v.op == nil {
		return // a fragment on its own: its variables are checked from each operation that uses it
	}
	name := val.Text
	v.used[name] = true
	vd := v.declared[name]
	if vd == nil {
		v.add(val.Line, val.Col, "variableNotDefined", "Variable $"+name+" is used by "+v.op.title()+" but not declared", "variableName", name)
		return
	}
	if vd.ref == nil || t == nil {
		return
	}
	loc := t
	if loc.NonNull && !vd.ref.NonNull && (hasDefault || (vd.Default != nil && vd.Default.Kind != ValNull)) {
		// A default on either side covers the missing value.
		loc = &TypeRef{Named: t.Named, Elem: t.Elem}
	}
	if what := mismatch(vd.ref, loc); what != "" {
		v.add(val.Line, val.Col, "variableMismatch", what+" mismatch on variable $"+name+" and argument "+position+" ("+vd.ref.String()+" / "+t.String()+")", "variableName", name, "typeName", vd.ref.String(), "argumentName", position)
	}
}

// mismatch says why a variable of type have cannot be used where want is expected ("" when it can).
func mismatch(have, want *TypeRef) string {
	switch {
	case want.NonNull && !have.NonNull:
		return "Nullability"
	case (have.Elem != nil) != (want.Elem != nil):
		return "List dimension"
	case have.Elem != nil:
		return mismatch(have.Elem, want.Elem)
	case have.Named != want.Named:
		return "Type"
	}
	return ""
}

// ---- field merging ----

// scope is a selection set together with the type it selects on.
type scope struct {
	parent *Type
	sel    []Selection
}

// occurrence is one field node that contributes to a response key.
type occurrence struct {
	f      *Field
	parent *Type
	def    *FieldDef
}

func (v *validator) flatten(sc scope, out []occurrence, seen map[string]bool) []occurrence {
	for _, sel := range sc.sel {
		v.work++
		switch n := sel.(type) {
		case *Field:
			if def := v.s.field(sc.parent, n.Name); def != nil {
				out = append(out, occurrence{n, sc.parent, def})
			}
		case *InlineFragment:
			t := sc.parent
			if n.On != "" {
				t = v.s.byName[n.On]
			}
			out = v.flatten(scope{t, n.Sel}, out, seen)
		case *FragmentSpread:
			if f := v.doc.frag[n.Name]; f != nil && !seen[n.Name] {
				seen[n.Name] = true
				out = v.flatten(scope{v.s.byName[f.On], f.Sel}, out, seen)
			}
		}
	}
	return out
}

func argumentsText(f *Field) string {
	parts := make([]string, len(f.Args))
	for i, a := range f.Args {
		parts[i] = a.Name + ":" + a.Value.String()
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ",") + "}"
}

// merge checks that the fields which will share a response key are the same field with the same
// arguments, here and in everything selected below. Fields on two different object types can never
// meet in one object, so those may differ. It runs only on a document with no other error.
func (v *validator) merge(scopes []scope) {
	if v.work > maxMergeWork {
		return
	}
	var all []occurrence
	seen := map[string]bool{}
	for _, sc := range scopes {
		all = v.flatten(sc, all, seen)
	}
	if v.work > maxMergeWork {
		v.add(0, 0, "tooComplex", "Query is too complex: its fragments expand to too many fields")
		return
	}
	done := make([]bool, len(all))
	for i, first := range all {
		if done[i] {
			continue
		}
		key := first.f.Key()
		var below []scope
		for j := i; j < len(all); j++ {
			o := all[j]
			if done[j] || o.f.Key() != key {
				continue
			}
			done[j] = true
			meet := o.parent == first.parent || o.parent.Kind != Object || first.parent.Kind != Object
			switch {
			case meet && o.f.Name != first.f.Name:
				v.add(o.f.Line, o.f.Col, "fieldConflict", "Field '"+key+"' has a field conflict: "+first.f.Name+" or "+o.f.Name+"?", "fieldName", key, "conflicts", first.f.Name+" or "+o.f.Name)
				continue
			case meet && argumentsText(o.f) != argumentsText(first.f):
				v.add(o.f.Line, o.f.Col, "fieldConflict", "Field '"+key+"' has an argument conflict: "+argumentsText(first.f)+" or "+argumentsText(o.f)+"?", "fieldName", key, "conflicts", argumentsText(first.f)+" or "+argumentsText(o.f))
				continue
			}
			if t := o.def.Type.named(); t.composite() {
				below = append(below, scope{t, o.f.Sel})
			}
		}
		if len(below) > 0 {
			v.push(key)
			v.merge(below)
			v.pop()
		}
	}
}
