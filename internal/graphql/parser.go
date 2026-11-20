package graphql

import (
	"strconv"
	"strings"

	"forge/internal/render"
)

// Document is a parsed executable document: operations and fragment definitions.
type Document struct {
	Ops   []*Operation
	Frags []*Fragment
	frag  map[string]*Fragment // the first definition of each name
}

// Operation is a query or a mutation.
type Operation struct {
	Kind string // "query", "mutation" or "subscription"
	Name string // "" for an anonymous operation
	Vars []*VarDef
	Dirs []*Directive
	Sel  []Selection
	Line int
	Col  int
}

// VarDef declares one variable of an operation.
type VarDef struct {
	Name    string
	Type    *TypeExpr
	Default *Value // nil when there is none
	Line    int
	Col     int

	ref *TypeRef // Type resolved against the schema; set by validation
}

// TypeExpr is a type as written in a document: a name or a list, each possibly non-null.
type TypeExpr struct {
	Name    string    // set for a named type
	Elem    *TypeExpr // set for a list
	NonNull bool
}

func (t *TypeExpr) String() string {
	s := t.Name
	if t.Elem != nil {
		s = "[" + t.Elem.String() + "]"
	}
	if t.NonNull {
		s += "!"
	}
	return s
}

// Selection is a *Field, an *InlineFragment or a *FragmentSpread.
type Selection interface{ selection() }

// Field is one field of a selection set.
type Field struct {
	Alias string
	Name  string
	Args  []*Argument
	Dirs  []*Directive
	Sel   []Selection
	Line  int
	Col   int
}

// Key is the name the field has in the response: its alias when it has one.
func (f *Field) Key() string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
}

// InlineFragment is "... on T { }" or, without a type condition, "... { }".
type InlineFragment struct {
	On   string
	Dirs []*Directive
	Sel  []Selection
	Line int
	Col  int
}

// FragmentSpread is "...Name".
type FragmentSpread struct {
	Name string
	Dirs []*Directive
	Line int
	Col  int
}

// Fragment is a fragment definition.
type Fragment struct {
	Name string
	On   string
	Dirs []*Directive
	Sel  []Selection
	Line int
	Col  int
}

func (*Field) selection()          {}
func (*InlineFragment) selection() {}
func (*FragmentSpread) selection() {}

// Directive is "@name(args)".
type Directive struct {
	Name string
	Args []*Argument
	Line int
	Col  int
}

// Argument is a named value: an argument of a field or directive, or a field of an object value.
type Argument struct {
	Name  string
	Value *Value
	Line  int
	Col   int
}

// ValueKind says which form a Value has.
type ValueKind uint8

const (
	ValVariable ValueKind = iota // Text is the variable's name
	ValInt                       // Text is the number as written
	ValFloat                     // Text is the number as written
	ValString                    // Text is the decoded string
	ValBool                      // Text is "true" or "false"
	ValNull
	ValEnum   // Text is the enum value's name
	ValList   // List
	ValObject // Fields
)

// Value is a value literal or a variable reference.
type Value struct {
	Kind   ValueKind
	Text   string
	List   []*Value
	Fields []*Argument
	Line   int
	Col    int
}

// String prints the value in GraphQL syntax.
func (v *Value) String() string { return string(v.append(nil)) }

func (v *Value) append(dst []byte) []byte {
	switch v.Kind {
	case ValVariable:
		dst = append(append(dst, '$'), v.Text...)
	case ValString:
		dst = render.AppendString(dst, v.Text)
	case ValNull:
		dst = append(dst, "null"...)
	case ValList:
		dst = append(dst, '[')
		for i, e := range v.List {
			if i > 0 {
				dst = append(dst, ", "...)
			}
			dst = e.append(dst)
		}
		dst = append(dst, ']')
	case ValObject:
		dst = append(dst, '{')
		for i, f := range v.Fields {
			if i > 0 {
				dst = append(dst, ", "...)
			}
			dst = append(append(dst, f.Name...), ": "...)
			dst = f.Value.append(dst)
		}
		dst = append(dst, '}')
	default:
		dst = append(dst, v.Text...)
	}
	return dst
}

// maxNesting bounds how deep selection sets, values and types may nest in one definition, so that
// no input can exhaust the stack of the recursive descent.
const maxNesting = 256

// parser is a recursive descent parser over the lexer's tokens. It reports the first error by
// panicking with an *Error, which parseDocument and parseSchema recover.
type parser struct {
	lex   lexer
	tok   token
	depth int
}

func (p *parser) advance() { p.tok = p.lex.next() }

var punctNames = map[string]string{
	"{": "LCURLY", "}": "RCURLY", "(": "LPAREN", ")": "RPAREN", "[": "LBRACKET", "]": "RBRACKET",
	":": "COLON", "$": "VAR_SIGN", "@": "DIR_SIGN", "...": "ELLIPSIS", "=": "EQUALS", "!": "BANG",
	"|": "PIPE", "&": "AMP",
}

// unexpected reports the current token as a syntax error.
func (p *parser) unexpected() {
	t := p.tok
	at := " at [" + strconv.Itoa(t.line) + ", " + strconv.Itoa(t.col) + "]"
	e := &Error{Line: t.line, Col: t.col}
	switch t.kind {
	case tokEOF:
		e.Message = "Unexpected end of document"
	case tokPunct:
		e.Message = `Parse error on "` + t.text + `" (` + punctNames[t.text] + ")" + at
	case tokName:
		e.Message = `Parse error on "` + t.text + `" (IDENTIFIER)` + at
	case tokInt:
		e.Message = "Parse error on " + t.text + " (INT)" + at
	case tokFloat:
		e.Message = "Parse error on " + t.text + " (FLOAT)" + at
	default:
		e.Message = "Parse error on " + strconv.Quote(t.text) + " (STRING)" + at
	}
	panic(e)
}

func (p *parser) enter() {
	if p.depth++; p.depth > maxNesting {
		panic(&Error{Message: "Document is nested too deeply (more than " + strconv.Itoa(maxNesting) + " levels)", Line: p.tok.line, Col: p.tok.col})
	}
}

func (p *parser) leave() { p.depth-- }

func (p *parser) isPunct(s string) bool { return p.tok.kind == tokPunct && p.tok.text == s }

// skip consumes the punctuator s when it is the current token.
func (p *parser) skip(s string) bool {
	if p.isPunct(s) {
		p.advance()
		return true
	}
	return false
}

func (p *parser) expect(s string) {
	if !p.skip(s) {
		p.unexpected()
	}
}

func (p *parser) isName(s string) bool { return p.tok.kind == tokName && p.tok.text == s }

// name consumes a Name token and returns it.
func (p *parser) name() string {
	if p.tok.kind != tokName {
		p.unexpected()
	}
	s := p.tok.text
	p.advance()
	return s
}

// recoverError turns a parser panic into the returned error and re-raises anything else.
func recoverError(err **Error) {
	if r := recover(); r != nil {
		e, ok := r.(*Error)
		if !ok {
			panic(r)
		}
		*err = e
	}
}

// parseDocument parses an executable document.
func parseDocument(src string) (doc *Document, err *Error) {
	defer recoverError(&err)
	p := &parser{lex: newLexer(src)}
	p.advance()
	doc = &Document{frag: map[string]*Fragment{}}
	for {
		switch {
		case p.isPunct("{"):
			op := &Operation{Kind: "query", Line: p.tok.line, Col: p.tok.col}
			op.Sel = p.selectionSet()
			doc.Ops = append(doc.Ops, op)
		case p.isName("query"), p.isName("mutation"), p.isName("subscription"):
			doc.Ops = append(doc.Ops, p.operation())
		case p.isName("fragment"):
			f := p.fragment()
			doc.Frags = append(doc.Frags, f)
			if doc.frag[f.Name] == nil {
				doc.frag[f.Name] = f
			}
		default:
			p.unexpected()
		}
		if p.tok.kind == tokEOF {
			return doc, nil
		}
	}
}

func (p *parser) operation() *Operation {
	op := &Operation{Kind: p.tok.text, Line: p.tok.line, Col: p.tok.col}
	p.advance()
	if p.tok.kind == tokName {
		op.Name = p.name()
	}
	if p.skip("(") {
		for {
			op.Vars = append(op.Vars, p.variableDefinition())
			if p.skip(")") {
				break
			}
		}
	}
	op.Dirs = p.directives(false)
	op.Sel = p.selectionSet()
	return op
}

func (p *parser) variableDefinition() *VarDef {
	v := &VarDef{Line: p.tok.line, Col: p.tok.col}
	p.expect("$")
	v.Name = p.name()
	p.expect(":")
	v.Type = p.typeExpr()
	if p.skip("=") {
		v.Default = p.value(true)
	}
	p.directives(true)
	return v
}

func (p *parser) typeExpr() *TypeExpr {
	t := &TypeExpr{}
	if p.skip("[") {
		p.enter()
		t.Elem = p.typeExpr()
		p.leave()
		p.expect("]")
	} else {
		t.Name = p.name()
	}
	t.NonNull = p.skip("!")
	return t
}

func (p *parser) fragment() *Fragment {
	f := &Fragment{Line: p.tok.line, Col: p.tok.col}
	p.advance()
	if p.isName("on") {
		p.unexpected()
	}
	f.Name = p.name()
	if !p.isName("on") {
		p.unexpected()
	}
	p.advance()
	f.On = p.name()
	f.Dirs = p.directives(false)
	f.Sel = p.selectionSet()
	return f
}

func (p *parser) selectionSet() []Selection {
	p.expect("{")
	p.enter()
	var sel []Selection
	for {
		sel = append(sel, p.selection())
		if p.skip("}") {
			break
		}
	}
	p.leave()
	return sel
}

func (p *parser) selection() Selection {
	line, col := p.tok.line, p.tok.col
	if p.skip("...") {
		if p.tok.kind == tokName && p.tok.text != "on" {
			s := &FragmentSpread{Name: p.name(), Line: line, Col: col}
			s.Dirs = p.directives(false)
			return s
		}
		f := &InlineFragment{Line: line, Col: col}
		if p.isName("on") {
			p.advance()
			f.On = p.name()
		}
		f.Dirs = p.directives(false)
		f.Sel = p.selectionSet()
		return f
	}
	f := &Field{Name: p.name(), Line: line, Col: col}
	if p.skip(":") {
		f.Alias, f.Name = f.Name, p.name()
	}
	if p.isPunct("(") {
		f.Args = p.arguments(false)
	}
	f.Dirs = p.directives(false)
	if p.isPunct("{") {
		f.Sel = p.selectionSet()
	}
	return f
}

// arguments parses "(name: value ...)"; constant forbids variables.
func (p *parser) arguments(constant bool) []*Argument {
	p.expect("(")
	var args []*Argument
	for {
		args = append(args, p.argument(constant))
		if p.skip(")") {
			return args
		}
	}
}

func (p *parser) argument(constant bool) *Argument {
	a := &Argument{Line: p.tok.line, Col: p.tok.col}
	a.Name = p.name()
	p.expect(":")
	a.Value = p.value(constant)
	return a
}

func (p *parser) directives(constant bool) []*Directive {
	var dirs []*Directive
	for p.isPunct("@") {
		d := &Directive{Line: p.tok.line, Col: p.tok.col}
		p.advance()
		d.Name = p.name()
		if p.isPunct("(") {
			d.Args = p.arguments(constant)
		}
		dirs = append(dirs, d)
	}
	return dirs
}

func (p *parser) value(constant bool) *Value {
	v := &Value{Text: p.tok.text, Line: p.tok.line, Col: p.tok.col}
	switch p.tok.kind {
	case tokInt:
		v.Kind = ValInt
	case tokFloat:
		v.Kind = ValFloat
	case tokString:
		v.Kind = ValString
	case tokName:
		switch v.Text {
		case "true", "false":
			v.Kind = ValBool
		case "null":
			v.Kind = ValNull
		default:
			v.Kind = ValEnum
		}
	case tokPunct:
		switch {
		case p.isPunct("$") && !constant:
			p.advance()
			v.Kind, v.Text = ValVariable, p.name()
			return v
		case p.isPunct("["):
			p.advance()
			p.enter()
			v.Kind, v.Text = ValList, ""
			for !p.skip("]") {
				v.List = append(v.List, p.value(constant))
			}
			p.leave()
			return v
		case p.isPunct("{"):
			p.advance()
			p.enter()
			v.Kind, v.Text = ValObject, ""
			for !p.skip("}") {
				v.Fields = append(v.Fields, p.argument(constant))
			}
			p.leave()
			return v
		}
		p.unexpected()
	default:
		p.unexpected()
	}
	p.advance()
	return v
}

// label names an operation in messages and error paths: "query Name", "mutation", ...
func (op *Operation) label() string {
	if op.Name == "" {
		return op.Kind
	}
	return op.Kind + " " + op.Name
}

// joinNames is strings.Join with ", ".
func joinNames(names []string) string { return strings.Join(names, ", ") }
