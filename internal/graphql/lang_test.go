package graphql

import (
	"encoding/json"
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"forge/internal/store"
)

// ---- lexer and parser ----

const bs = `\`

func TestLexer(t *testing.T) {
	type tok struct {
		kind tokenKind
		text string
	}
	lex := func(src string) (out []tok, err *Error) {
		defer recoverError(&err)
		l := newLexer(src)
		for {
			tk := l.next()
			if tk.kind == tokEOF {
				return out, nil
			}
			out = append(out, tok{tk.kind, tk.text})
		}
	}
	good := []struct {
		src  string
		want []tok
	}{
		{"", nil},
		{" ,\t\r\n# only a comment", nil},
		{"{a_1:b}", []tok{{tokPunct, "{"}, {tokName, "a_1"}, {tokPunct, ":"}, {tokName, "b"}, {tokPunct, "}"}}},
		{"...on $x @d [T!]=|&()", []tok{{tokPunct, "..."}, {tokName, "on"}, {tokPunct, "$"}, {tokName, "x"}, {tokPunct, "@"}, {tokName, "d"},
			{tokPunct, "["}, {tokName, "T"}, {tokPunct, "!"}, {tokPunct, "]"}, {tokPunct, "="}, {tokPunct, "|"}, {tokPunct, "&"}, {tokPunct, "("}, {tokPunct, ")"}}},
		{"0 -0 12 -345 1.5 -0.25 1e10 2E-3 6.02e+23", []tok{{tokInt, "0"}, {tokInt, "-0"}, {tokInt, "12"}, {tokInt, "-345"}, {tokFloat, "1.5"}, {tokFloat, "-0.25"}, {tokFloat, "1e10"}, {tokFloat, "2E-3"}, {tokFloat, "6.02e+23"}}},
		{`"" "plain" "sp ace, #not a comment"`, []tok{{tokString, ""}, {tokString, "plain"}, {tokString, "sp ace, #not a comment"}}},
		{`"q` + bs + `" b` + bs + bs + ` s` + bs + `/ ` + bs + `b` + bs + `f` + bs + `n` + bs + `r` + bs + `t"`, []tok{{tokString, "q\" b\\ s/ \b\f\n\r\t"}}},
		{`"` + bs + `u00e9` + bs + `u4E16 ` + bs + `uD83D` + bs + `uDE00 ` + bs + `u{1F600} ` + bs + `u{e9}"`, []tok{{tokString, "\xc3\xa9\xe4\xb8\x96 \xf0\x9f\x98\x80 \xf0\x9f\x98\x80 \xc3\xa9"}}},
		{`"""block"""`, []tok{{tokString, "block"}}},
		{"\"\"\"\n    first\n      indented\n    last \"quoted\" " + bs + "\"\"\" done\n\n  \"\"\"", []tok{{tokString, "first\n  indented\nlast \"quoted\" \"\"\" done"}}},
		{"\"\"\"one\r\n  two\r  three\"\"\" x", []tok{{tokString, "one\ntwo\nthree"}, {tokName, "x"}}},
		{`""""""`, []tok{{tokString, ""}}},
	}
	for _, c := range good {
		got, err := lex(c.src)
		if err != nil || len(got) != len(c.want) {
			t.Errorf("lex(%q) = %v, %v; want %v", c.src, got, err, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("lex(%q)[%d] = %v, want %v", c.src, i, got[i], c.want[i])
			}
		}
	}
	bad := map[string]string{
		"01":                    "leading zero",
		"1.":                    "malformed number",
		"1.e3":                  "malformed number",
		"1e":                    "malformed number",
		"-":                     "malformed number",
		"1x":                    "malformed number",
		"1.5.2":                 "malformed number",
		".5":                    `Parse error on "."`,
		"..":                    `Parse error on "."`,
		`"open`:                 "unterminated string",
		"\"line\nbreak\"":       "unterminated string",
		`"` + bs + `q"`:         "bad escape",
		`"` + bs + `u12"`:       "bad Unicode escape",
		`"` + bs + `uZZZZ"`:     "bad Unicode escape",
		`"` + bs + `u{}"`:       "bad Unicode escape",
		`"` + bs + `u{110000}"`: "bad Unicode escape",
		`"` + bs:                "unterminated string",
		`"""open`:               "unterminated block string",
		"a ? b":                 `Parse error on "?"`,
		"a \x00":                `Parse error on "\x00"`,
	}
	for src, want := range bad {
		if got, err := lex(src); err == nil || !strings.Contains(err.Message, want) {
			t.Errorf("lex(%q) = %v, %v; want an error about %q", src, got, err, want)
		}
	}
}

func TestPositions(t *testing.T) {
	// Lines end with \n, \r\n or \r; columns count characters, not bytes; a byte order mark has
	// no width.
	doc, err := parseDocument("\xEF\xBB\xBFquery {\r\n  a: first\r  \"caf\xc3\xa9\" second(x: \"\"\"\n\n\"\"\") {\n\tthird }\n}")
	if err == nil {
		t.Fatalf("a string is not a selection, but the document parsed: %+v", doc)
	}
	if err.Line != 3 || err.Col != 3 || err.Message != `Parse error on "café" (STRING) at [3, 3]` {
		t.Errorf("error = %+v", err)
	}
	doc, err = parseDocument("query {\r\n  a: first\r  # caf\xc3\xa9\n  \xc3\xa9: second(x: \"\"\"\n\n\"\"\") {\n\tthird }\n}")
	if err == nil || err.Line != 4 || err.Col != 3 {
		t.Errorf("non-ASCII outside a string: %+v %+v", doc, err)
	}
	doc, err = parseDocument("query {\r\n  a: first\r  second(x: \"\"\"\n\n\"\"\", y: \"\xc3\xa9\xc3\xa9\") {\n\tthird }\n}")
	if err != nil {
		t.Fatal(err)
	}
	sel := doc.Ops[0].Sel
	first, second := sel[0].(*Field), sel[1].(*Field)
	third := second.Sel[0].(*Field)
	if first.Line != 2 || first.Col != 3 || first.Alias != "a" || first.Name != "first" ||
		second.Line != 3 || second.Col != 3 || second.Args[1].Line != 5 || second.Args[1].Col != 6 ||
		third.Line != 6 || third.Col != 2 || doc.Ops[0].Line != 1 || doc.Ops[0].Col != 1 {
		t.Errorf("positions: first %d:%d second %d:%d y %d:%d third %d:%d", first.Line, first.Col, second.Line, second.Col, second.Args[1].Line, second.Args[1].Col, third.Line, third.Col)
	}
}

func TestParser(t *testing.T) {
	doc, err := parseDocument(`
query Q($a: Int = 1, $b: [String!]! = ["x"], $c: In = {k: [1, 2.5, true, null, E, "s", {n: 3}]}) @dir(x: 1) {
  alias: field(arg: $a, list: [], obj: {}) @skip(if: false) @other { ...Spread @x ... on T @y { leaf } ... { on } }
  on: query
}
mutation { fragment(on: null) }
fragment Spread on T @z { true: false }
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Ops) != 2 || len(doc.Frags) != 1 || doc.frag["Spread"] == nil || doc.Frags[0].On != "T" {
		t.Fatalf("definitions: %d operations, %d fragments", len(doc.Ops), len(doc.Frags))
	}
	q := doc.Ops[0]
	if q.Kind != "query" || q.Name != "Q" || q.label() != "query Q" || doc.Ops[1].label() != "mutation" || len(q.Dirs) != 1 {
		t.Errorf("operation: %+v", q)
	}
	var types, defaults []string
	for _, v := range q.Vars {
		types = append(types, v.Name+": "+v.Type.String())
		defaults = append(defaults, v.Default.String())
	}
	if got := strings.Join(types, "; "); got != "a: Int; b: [String!]!; c: In" {
		t.Errorf("variable types = %s", got)
	}
	if got := strings.Join(defaults, "; "); got != `1; ["x"]; {k: [1, 2.5, true, null, E, "s", {n: 3}]}` {
		t.Errorf("variable defaults = %s", got)
	}
	f := q.Sel[0].(*Field)
	if f.Key() != "alias" || f.Name != "field" || len(f.Args) != 3 || f.Args[1].Value.Kind != ValList || f.Args[2].Value.Kind != ValObject || len(f.Dirs) != 2 || len(f.Sel) != 3 {
		t.Errorf("field: %+v", f)
	}
	if s, ok := f.Sel[0].(*FragmentSpread); !ok || s.Name != "Spread" || len(s.Dirs) != 1 {
		t.Errorf("spread: %+v", f.Sel[0])
	}
	if in, ok := f.Sel[1].(*InlineFragment); !ok || in.On != "T" || len(in.Dirs) != 1 {
		t.Errorf("inline fragment: %+v", f.Sel[1])
	}
	// Keywords are ordinary names wherever a name is expected.
	if in, ok := f.Sel[2].(*InlineFragment); !ok || in.On != "" || in.Sel[0].(*Field).Name != "on" {
		t.Errorf("untyped inline fragment: %+v", f.Sel[2])
	}
	if k := q.Sel[1].(*Field); k.Alias != "on" || k.Name != "query" {
		t.Errorf("keyword field: %+v", k)
	}

	for src, want := range map[string]string{
		"":                                   "Unexpected end of document",
		"query":                              "Unexpected end of document",
		"{ a":                                "Unexpected end of document",
		"{ }":                                `Parse error on "}" (RCURLY) at [1, 3]`,
		"{ a( ) }":                           `Parse error on ")" (RPAREN) at [1, 6]`,
		"{ a(b) }":                           `Parse error on ")" (RPAREN) at [1, 6]`,
		"{ a(b: ) }":                         `Parse error on ")" (RPAREN) at [1, 8]`,
		"{ a: }":                             `Parse error on "}" (RCURLY) at [1, 6]`,
		"{ a } }":                            `Parse error on "}" (RCURLY) at [1, 7]`,
		"{ a } extra":                        `Parse error on "extra" (IDENTIFIER) at [1, 7]`,
		"query ($a) { a }":                   `Parse error on ")" (RPAREN) at [1, 10]`,
		"query ($a: [Int) { a }":             `Parse error on ")" (RPAREN) at [1, 16]`,
		"query ($a: Int = $b) { a }":         `Parse error on "$" (VAR_SIGN) at [1, 18]`,
		"query () { a }":                     `Parse error on ")" (RPAREN) at [1, 8]`,
		"fragment on on T { a }":             `Parse error on "on" (IDENTIFIER) at [1, 10]`,
		"fragment F { a }":                   `Parse error on "{" (LCURLY) at [1, 12]`,
		"{ ... }":                            `Parse error on "}" (RCURLY) at [1, 7]`,
		"{ a(b: [1, ) }":                     `Parse error on ")" (RPAREN) at [1, 12]`,
		"{ a(b: {c}) }":                      `Parse error on "}" (RCURLY) at [1, 10]`,
		"{ a @ }":                            `Parse error on "}" (RCURLY) at [1, 7]`,
		"{ 1 }":                              "Parse error on 1 (INT) at [1, 3]",
		"{ a(b: 1.5 c) }":                    `Parse error on ")" (RPAREN) at [1, 13]`,
		"type T { a: Int }":                  `Parse error on "type" (IDENTIFIER) at [1, 1]`,
		strings.Repeat("{ a ", maxNesting+1): "nested too deeply",
		"{ a(b: " + strings.Repeat("[", maxNesting+1): "nested too deeply",
	} {
		if doc, err := parseDocument(src); err == nil || !strings.Contains(err.Message, want) {
			t.Errorf("parse(%.40q) = %v, %v; want an error about %q", src, doc, err, want)
		}
	}
}

// ---- executor, on a schema made for the purpose ----

const miniSDL = `
schema { query: Q }
scalar String scalar Int scalar Float scalar Boolean scalar ID
directive @include(if: Boolean!) on FIELD
directive @skip(if: Boolean!) on FIELD
enum Color { RED GREEN }
interface Shape { name: String! }
union Thing = Box | Ball
type Box implements Shape { name: String!  must: String!  inner: Box }
type Ball implements Shape { name: String! }
type Q {
  ok: String
  must: String!
  box(must: Boolean = true): Box
  strict: Box!
  strictList: [Box!]
  looseList: [Box]
  both: [Box!]!
  fails: String
  failsHard: String!
  panics: String
  wrongInt: Int
  bigInt: Int
  wrongEnum: Color
  wrongList: [String]
  notAThing: Thing
  shapes: [Shape!]!
  things: [Thing]
  color: Color
  float: Float
  many(n: Int!): [Int!]!
  echo(i: Int = 3, f: Float, s: String = "dflt", b: Boolean, id: ID, c: Color = GREEN, l: [Int!], o: In): String!
}
input In { a: Int!  b: [Color!] = [RED]  c: In }
`

type box struct{ must bool }
type ball struct{}

func miniAPI(t *testing.T) *API {
	t.Helper()
	s, perr := parseSchema(miniSDL)
	if perr != nil {
		t.Fatal(perr)
	}
	boxes := func(*query, root) any { return []any{&box{true}, &box{false}, &box{true}} }
	get(s, "Q", getters[root]{
		"ok":         func(*query, root) any { return "fine" },
		"must":       func(*query, root) any { return nil },
		"strict":     func(*query, root) any { return &box{false} },
		"strictList": boxes,
		"looseList":  boxes,
		"both":       boxes,
		"panics":     func(*query, root) any { panic("resolver bug") },
		"wrongInt":   func(*query, root) any { return "seven" },
		"bigInt":     func(*query, root) any { return int64(1) << 40 },
		"wrongEnum":  func(*query, root) any { return "BLUE" },
		"wrongList":  func(*query, root) any { return "not a list" },
		"notAThing":  func(*query, root) any { return 42 },
		"shapes":     func(*query, root) any { return []any{&box{true}, ball{}} },
		"things":     func(*query, root) any { return []any{ball{}, nil, &box{true}, (*box)(nil)} },
		"color":      func(*query, root) any { return "RED" },
		"float":      func(*query, root) any { return 2.5 },
	})
	call(s, "Q", callers[root]{
		"box":       func(_ *query, _ root, a args) (any, error) { return &box{a.flag("must")}, nil },
		"fails":     func(*query, root, args) (any, error) { return "ignored", errors.New("it broke") },
		"failsHard": func(*query, root, args) (any, error) { return nil, &store.Error{Status: 404, Message: "Not Found"} },
		"many": func(_ *query, _ root, a args) (any, error) {
			n, _ := a.int("n")
			return make([]any, n), nil
		},
		"echo": func(_ *query, _ root, a args) (any, error) {
			var parts []string
			for _, k := range []string{"i", "f", "s", "b", "id", "c", "l", "o"} {
				if v, given := a[k]; given {
					parts = append(parts, k+"="+strings.ReplaceAll(strings.ReplaceAll(jsonText(describe(v)), `"`, ""), "graphql.", ""))
				}
			}
			return strings.Join(parts, " "), nil
		},
	})
	get(s, "Box", getters[*box]{
		"name": func(*query, *box) any { return "box" },
		"must": func(_ *query, b *box) any {
			if b.must {
				return "have"
			}
			return nil
		},
		"inner": func(_ *query, b *box) any { return &box{!b.must} },
	})
	get(s, "Ball", getters[ball]{"name": func(*query, ball) any { return "ball" }})
	s.link()
	if err := s.check(); err != nil {
		t.Fatal(err)
	}
	return &API{schema: s, prepared: map[string]*prepared{}}
}

// describe makes a coerced value printable with its Go types visible where they matter.
func describe(v any) any {
	switch x := v.(type) {
	case args:
		out := map[string]any{}
		for k, e := range x {
			out[k] = describe(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = describe(e)
		}
		return out
	case float64:
		return "float:" + strconv.FormatFloat(x, 'g', -1, 64)
	}
	return v
}

func TestExecutor(t *testing.T) {
	api := miniAPI(t)
	run := func(query string, vars map[string]any) string {
		return string(api.run(nil, "h", &request{query: query, hasQuery: true, variables: vars}))
	}
	const nonNull = `"message":"Cannot return null for non-nullable field `
	cases := []struct {
		name  string
		query string
		vars  map[string]any
		want  string
	}{
		{"plain", `{ ok color float }`, nil, `{"data":{"ok":"fine","color":"RED","float":2.5}}`},
		{"a null in a non-null root field takes the data", `{ ok must }`, nil,
			`{"data":null,"errors":[{"type":"INTERNAL","path":["must"],"locations":[{"line":1,"column":6}],` + nonNull + `Q.must"}]}`},
		{"a null below a nullable field stops there", `{ ok box(must: false) { name must } after: ok }`, nil,
			`{"data":{"ok":"fine","box":null,"after":"fine"},"errors":[{"type":"INTERNAL","path":["box","must"],"locations":[{"line":1,"column":30}],` + nonNull + `Box.must"}]}`},
		{"it rises through every non-null parent", `{ a: box { name inner { inner { must } } } strict { name } }`, nil,
			`{"data":{"a":{"name":"box","inner":{"inner":{"must":"have"}}},"strict":{"name":"box"}}}`},
		{"up to the root", `{ ok strict { must } }`, nil,
			`{"data":null,"errors":[{"type":"INTERNAL","path":["strict","must"],"locations":[{"line":1,"column":15}],` + nonNull + `Box.must"}]}`},
		{"a failed element of a non-null element list takes the list", `{ strictList { must } ok }`, nil,
			`{"data":{"strictList":null,"ok":"fine"},"errors":[{"type":"INTERNAL","path":["strictList",1,"must"],"locations":[{"line":1,"column":16}],` + nonNull + `Box.must"}]}`},
		{"a nullable element becomes null alone", `{ looseList { must } }`, nil,
			`{"data":{"looseList":[{"must":"have"},null,{"must":"have"}]},"errors":[{"type":"INTERNAL","path":["looseList",1,"must"],"locations":[{"line":1,"column":15}],` + nonNull + `Box.must"}]}`},
		{"a non-null list of non-null elements takes its parent", `{ both { must } }`, nil,
			`{"data":null,"errors":[{"type":"INTERNAL","path":["both",1,"must"],"locations":[{"line":1,"column":10}],` + nonNull + `Box.must"}]}`},
		{"a resolver error nulls the field and is reported once", `{ fails ok }`, nil,
			`{"data":{"fails":null,"ok":"fine"},"errors":[{"type":"INTERNAL","path":["fails"],"locations":[{"line":1,"column":3}],"message":"it broke"}]}`},
		{"a store error keeps its message and maps its status", `{ ok failsHard }`, nil,
			`{"data":null,"errors":[{"type":"NOT_FOUND","path":["failsHard"],"locations":[{"line":1,"column":6}],"message":"Not Found"}]}`},
		{"values of the wrong Go type are errors, not output", `{ wrongInt bigInt wrongEnum wrongList notAThing { __typename } ok }`, nil,
			`{"data":{"wrongInt":null,"bigInt":null,"wrongEnum":null,"wrongList":null,"notAThing":null,"ok":"fine"},"errors":[` +
				`{"type":"INTERNAL","path":["wrongInt"],"locations":[{"line":1,"column":3}],"message":"The resolver of Q.wrongInt returned a value that is no Int"},` +
				`{"type":"INTERNAL","path":["bigInt"],"locations":[{"line":1,"column":12}],"message":"The resolver of Q.bigInt returned a value that is no Int"},` +
				`{"type":"INTERNAL","path":["wrongEnum"],"locations":[{"line":1,"column":19}],"message":"The resolver of Q.wrongEnum returned a value that is no Color"},` +
				`{"type":"INTERNAL","path":["wrongList"],"locations":[{"line":1,"column":29}],"message":"The resolver of Q.wrongList returned a value that is not a list"},` +
				`{"type":"INTERNAL","path":["notAThing"],"locations":[{"line":1,"column":39}],"message":"The resolver of Q.notAThing returned a value that is no Thing"}]}`},
		{"a panic in a resolver is the operation's error", `{ ok panics }`, nil,
			`{"errors":[{"message":"Something went wrong while executing your query."}]}`},
		{"interfaces and unions resolve to the value's type", `{ shapes { __typename name ... on Box { must } } things { __typename ... on Ball { name } ... on Shape { n: name } } }`, nil,
			`{"data":{"shapes":[{"__typename":"Box","name":"box","must":"have"},{"__typename":"Ball","name":"ball"}],"things":[{"__typename":"Ball","name":"ball","n":"ball"},null,{"__typename":"Box","n":"box"},null]}}`},
		{"argument defaults", `{ echo }`, nil, `{"data":{"echo":"i=3 s=dflt c=GREEN"}}`},
		{"argument literals", `{ echo(i: -7, f: 2, s: "x", b: true, id: 12, c: RED, l: 5, o: {a: 1, c: {a: 2, b: []}}) }`, nil,
			`{"data":{"echo":"i=-7 f=float:2 s=x b=true id=12 c=RED l=[5] o={a:1,b:[RED],c:{a:2,b:[]}}"}}`},
		{"explicit nulls are kept apart from absence", `{ echo(i: null, s: null, f: null) }`, nil, `{"data":{"echo":"i=null f=null s=null c=GREEN"}}`},
		{"variables", `query($i: Int, $f: Float = 1.5, $id: ID, $l: [Int!], $o: In, $c: Color, $s: String) { echo(i: $i, f: $f, id: $id, l: $l, o: $o, c: $c, s: $s) }`,
			map[string]any{"i": num("4"), "id": num("99"), "l": num("6"), "o": map[string]any{"a": num("1.0"), "b": "GREEN", "unknown": true}, "c": nil},
			`{"data":{"echo":"i=4 f=float:1.5 s=dflt id=99 c=null l=[6] o={a:1,b:[GREEN]}"}}`},
		{"an absent variable inside an input object falls back to the default", `query($b: [Color!], $a: Int = 9) { echo(o: {a: $a, b: $b}) }`, nil,
			`{"data":{"echo":"i=3 s=dflt c=GREEN o={a:9,b:[RED]}"}}`},
		{"a null variable for a non-null input field fails that field only", `query($a: Int = 1) { echo(o: {a: $a}) ok }`, map[string]any{"a": nil},
			`{"data":null,"errors":[{"path":["echo"],"locations":[{"line":1,"column":22}],"message":"Argument 'o' on Field 'echo' has an invalid value ({a: $a}). Expected type 'In'."}]}`},
		{"the response is cut off at the field limit", `{ many(n: 600000) }`, nil,
			`{"errors":[{"message":"The query asks for more than 500000 fields and list items."}]}`},
		{"directives decide per request", `query($on: Boolean!) { ok @include(if: $on) color @skip(if: $on) }`, map[string]any{"on": false}, `{"data":{"color":"RED"}}`},
	}
	for _, c := range cases {
		if got := run(c.query, c.vars); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
	for name, c := range map[string]struct{ query, want string }{
		"Int out of range":         {`{ echo(i: 3000000000) }`, "Argument 'i' on Field 'echo' has an invalid value (3000000000). Expected type 'Int'."},
		"Float for an Int":         {`{ echo(i: 1.0) }`, "Argument 'i' on Field 'echo' has an invalid value (1.0). Expected type 'Int'."},
		"String for an ID is fine": {`{ echo(id: 1.5) }`, "Argument 'id' on Field 'echo' has an invalid value (1.5). Expected type 'ID'."},
		"missing input field":      {`{ echo(o: {b: [RED]}) }`, "Argument 'o' on Field 'echo' has an invalid value ({b: [RED]}). Expected type 'In'."},
		"nested input object":      {`{ echo(o: {a: 1, c: {a: "x"}}) }`, `Argument 'o' on Field 'echo' has an invalid value ({a: 1, c: {a: "x"}}). Expected type 'In'.`},
		"list of the wrong things": {`{ echo(l: [1, "2"]) }`, `Argument 'l' on Field 'echo' has an invalid value ([1, "2"]). Expected type '[Int!]'.`},
		"null in a non-null list":  {`{ echo(l: [1, null]) }`, "Argument 'l' on Field 'echo' has an invalid value ([1, null]). Expected type '[Int!]'."},
		"list variable in an item": {`query($l: [Int!]) { echo(l: [$l]) }`, "List dimension mismatch on variable $l and argument l ([Int!] / Int!)"},
		"nullable item variable":   {`query($i: Int) { echo(l: [$i]) }`, "Nullability mismatch on variable $i and argument l (Int / Int!)"},
		"variable in an object":    {`query($s: String!) { echo(o: {a: $s}) }`, "Type mismatch on variable $s and argument a (String! / Int!)"},
		"depth":                    {`{ box ` + strings.Repeat("{ inner ", maxDepth) + "{ name }" + strings.Repeat(" }", maxDepth) + ` }`, "Query has depth of 202, which exceeds max depth of 200"},
	} {
		if got := run(c.query, nil); !strings.Contains(got, jsonText(c.want)) || strings.Contains(got, `"data"`) {
			t.Errorf("%s:\n got %s\nwant an error saying %s", name, got, c.want)
		}
	}
	// A default lets a nullable variable stand in a non-null position.
	if got := run(`query($n: Int = 2) { many(n: $n) }`, nil); got != `{"data":null,"errors":[{"type":"INTERNAL","path":["many",0],"locations":[{"line":1,"column":22}],`+nonNull+`Q.many"}]}` {
		t.Errorf("default for a non-null position: %s", got)
	}
}

func num(s string) any { return json.Number(s) }

// The work a document can cause is bounded however its fragments multiply.
func TestFragmentBomb(t *testing.T) {
	api := miniAPI(t)
	var b strings.Builder
	b.WriteString("{ box { ...F0 } }")
	for i := 0; i < 40; i++ {
		next := "name"
		if i < 39 {
			next = "...F" + strconv.Itoa(i+1) + " inner { ...F" + strconv.Itoa(i+1) + " }"
		}
		b.WriteString(" fragment F" + strconv.Itoa(i) + " on Box { a" + strconv.Itoa(i) + ": inner { " + next + " } }")
	}
	got := string(api.run(nil, "h", &request{query: b.String(), hasQuery: true}))
	if !strings.Contains(got, "Query is too complex") && !strings.Contains(got, "asks for more than") {
		t.Errorf("fragment bomb: %.300s", got)
	}
}

// ---- robustness: nothing a client sends may panic or produce anything but a reply object ----

func TestGarbage(t *testing.T) {
	h := newHarness(t)
	seed(t, h)
	docs := []string{
		viewerLogin, repositoryFields, issueFields, typeFields, openIssues, issueByNumber, createIssue, closeIssue, pullRequests,
		pullRequestFiles, createPullRequest, mergePullRequest, addComment, invalidField, nodeRepository, issuesPage, introspectionQuery,
		`query($s: [IssueState!] = [OPEN], $o: IssueOrder = {field: COMMENTS, direction: ASC}) { ` + inSeeded + ` { issues(states: $s, orderBy: $o, first: 2) { edges { cursor node { ...I } } } } } fragment I on Issue { id title labels(first: 1) { nodes { name } } }`,
		`{ search(query: "alpha is:issue", type: ISSUE, first: 3) { nodes { __typename ... on Node { id } } } }`,
	}
	const alphabet = "{}()[]:!$@=|&.,#\"\\ \n\t-0123456789eEabcdefinoqrstuy_"
	rng := rand.New(rand.NewSource(1))
	vars := []any{nil, map[string]any{"owner": "adaiversen", "name": "seeded", "first": 2, "number": 1, "id": "x", "input": map[string]any{}}, map[string]any{"first": "x", "input": 3, "after": 1.5}}
	count := 0
	for _, doc := range docs {
		for i := 0; i < 400; i++ {
			b := []byte(doc)
			for edits := 1 + rng.Intn(3); edits > 0; edits-- {
				at := rng.Intn(len(b) + 1)
				switch rng.Intn(4) {
				case 0: // delete a run
					end := min(len(b), at+1+rng.Intn(8))
					b = append(b[:at:at], b[end:]...)
				case 1: // insert a character
					b = append(b[:at:at], append([]byte{alphabet[rng.Intn(len(alphabet))]}, b[at:]...)...)
				case 2: // duplicate a run
					end := min(len(b), at+1+rng.Intn(12))
					b = append(b[:end:end], append(append([]byte{}, b[at:end]...), b[end:]...)...)
				case 3: // truncate
					b = b[:at]
				}
			}
			status, reply := h.send("POST", testToken, requestBody(string(b), vars[rng.Intn(len(vars))]))
			r := decodeReply(t, reply)
			_, hasData := r.keys["data"]
			if status != 200 || (!hasData && len(r.errors) == 0) || strings.Contains(r.raw, "Something went wrong") {
				t.Fatalf("mutated document %q: %d %s", b, status, reply)
			}
			count++
		}
	}
	if count != len(docs)*400 {
		t.Fatalf("ran %d documents", count)
	}
}
