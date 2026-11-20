package graphql

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"forge/internal/store"
)

// ---- the schema and its introspection ----

func TestSchemaBuilds(t *testing.T) {
	s, err := schema()
	if err != nil {
		t.Fatal(err)
	}
	if s.byName[metaType] != nil {
		t.Errorf("the meta pseudo-type is still a type of the schema")
	}
	for _, typ := range s.Types {
		switch typ.Kind {
		case Object, Interface:
			if len(typ.Fields) == 0 {
				t.Errorf("%s has no fields", typ.Name)
			}
		case Union:
			if len(typ.Possible) == 0 {
				t.Errorf("union %s has no members", typ.Name)
			}
		case Enum:
			if len(typ.Values) == 0 {
				t.Errorf("enum %s has no values", typ.Name)
			}
		case InputObject:
			if len(typ.Inputs) == 0 {
				t.Errorf("input %s has no fields", typ.Name)
			}
		}
		if typ.Kind == Interface && len(typ.Possible) == 0 {
			t.Errorf("no object implements %s", typ.Name)
		}
	}
	// Every Go type behind an interface or union resolves to its object type.
	for value, want := range map[any]string{
		store.Viewer{}: "User", &store.Repo{}: "Repository", &store.Issue{}: "Issue", &store.Pull{}: "PullRequest",
		&store.Comment{}: "IssueComment", &store.Label{}: "Label", &ref{}: "Ref", &commit{}: "Commit",
	} {
		if got := s.typeOf(value); got == nil || got.Name != want {
			t.Errorf("typeOf(%T) = %v, want %s", value, got, want)
		}
	}
}

// A definition error must fail the build rather than surface at request time.
func TestSchemaProblems(t *testing.T) {
	const base = `schema { query: Q } scalar String scalar Int `
	build := func(sdl string, register func(s *Schema)) error {
		s, perr := parseSchema(base + sdl)
		if perr != nil {
			return perr
		}
		if register != nil {
			register(s)
		}
		s.link()
		return s.check()
	}
	a := getters[root]{"a": func(*query, root) any { return "a" }}
	cases := []struct {
		name     string
		sdl      string
		register func(s *Schema)
		want     string
	}{
		{"field without resolver", `type Q { a: String b: String }`, func(s *Schema) { get(s, "Q", a) }, "no resolver for Q.b"},
		{"resolver without field", `type Q { a: String }`, func(s *Schema) {
			get(s, "Q", a)
			get(s, "Q", getters[root]{"zz": func(*query, root) any { return 1 }})
		}, "resolver for Q.zz, which the schema does not define"},
		{"resolver for an unknown type", `type Q { a: String }`, func(s *Schema) {
			get(s, "Q", a)
			get(s, "Nope", a)
		}, "resolver for Nope.a, a type the schema does not define"},
		{"two resolvers", `type Q { a: String }`, func(s *Schema) {
			get(s, "Q", a)
			get(s, "Q", a)
		}, "two resolvers for Q.a"},
		{"unknown field type", `type Q { a: Nope }`, func(s *Schema) { get(s, "Q", a) }, "Q.a has the unknown type Nope"},
		{"unknown argument type", `type Q { a(x: [Nope!]): String }`, func(s *Schema) { get(s, "Q", a) }, "Q.a.x has the unknown type [Nope!]"},
		{"output type as an argument", `type Q { a(x: Q): String }`, func(s *Schema) { get(s, "Q", a) }, "Q.a.x is not of an input type"},
		{"default of the wrong type", `type Q { a(x: Int = "one"): String }`, func(s *Schema) { get(s, "Q", a) }, "the default of Q.a.x does not fit its type"},
		{"interface field missing", `interface I { a: String b: Int } type Q implements I { a: String }`, func(s *Schema) { get(s, "Q", a) }, "Q implements I but has no field b"},
		{"not an interface", `type Q implements String { a: String }`, func(s *Schema) { get(s, "Q", a) }, "Q implements String, which is not an interface"},
		{"union of a scalar", `type Q { a: String } union U = Q | Int`, func(s *Schema) { get(s, "Q", a) }, "union U has the member Int, which is not an object type"},
		{"duplicate type", `type Q { a: String } scalar Int`, func(s *Schema) { get(s, "Q", a) }, "type Int is defined twice"},
		{"no query root", `type R { a: String }`, nil, "the schema has no query root type"},
	}
	for _, c := range cases {
		err := build(c.sdl, c.register)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want it to mention %q", c.name, err, c.want)
		}
	}
	if err := build(`type Q { a: String }`, func(s *Schema) { get(s, "Q", a) }); err != nil {
		t.Errorf("a correct schema does not build: %v", err)
	}
	if _, perr := parseSchema(`type Q { a String }`); perr == nil || !strings.Contains(perr.Message, "Parse error") {
		t.Errorf("a syntax error in a schema: %v", perr)
	}
}

// introspectionQuery asks for everything the introspection types offer, as schema-aware clients do
// before their first real request.
const introspectionQuery = `query IntrospectionQuery {
  __schema {
    description
    queryType { name }
    mutationType { name }
    subscriptionType { name }
    types { ...FullType }
    directives { name description locations isRepeatable args { ...InputValue } }
  }
}
fragment FullType on __Type {
  kind name description specifiedByURL
  fields(includeDeprecated: true) { name description args { ...InputValue } type { ...TypeRef } isDeprecated deprecationReason }
  inputFields { ...InputValue }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) { name description isDeprecated deprecationReason }
  possibleTypes { ...TypeRef }
}
fragment InputValue on __InputValue { name description type { ...TypeRef } defaultValue isDeprecated deprecationReason }
fragment TypeRef on __Type {
  kind name
  ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } }
}`

func TestIntrospection(t *testing.T) {
	h := newHarness(t)
	sch := h.data(introspectionQuery, nil)["__schema"].(map[string]any)
	if at(sch, "queryType", "name") != "Query" || at(sch, "mutationType", "name") != "Mutation" || sch["subscriptionType"] != nil {
		t.Fatalf("root types: %v %v %v", sch["queryType"], sch["mutationType"], sch["subscriptionType"])
	}
	types := map[string]map[string]any{}
	var order []string
	for _, typ := range sch["types"].([]any) {
		name := at(typ, "name").(string)
		types[name] = typ.(map[string]any)
		order = append(order, name)
	}
	if !sort.StringsAreSorted(order) {
		t.Errorf("types are not sorted by name: %v", order)
	}
	for _, name := range []string{"Query", "Mutation", "Repository", "Issue", "PullRequest", "User", "Node", "SearchResultItem", "IssueState", "CreateIssueInput", "String", "Int", "Boolean", "ID", "Float", "DateTime", "__Schema", "__Type", "__TypeKind", "__DirectiveLocation"} {
		if types[name] == nil {
			t.Errorf("type %s is missing", name)
		}
	}
	if types[metaType] != nil {
		t.Errorf("the meta pseudo-type is listed")
	}

	// Every reference anywhere ends at a listed type, and the kind it states is that type's kind.
	var checkRef func(where string, ref any)
	checkRef = func(where string, ref any) {
		kind, _ := at(ref, "kind").(string)
		if kind == "LIST" || kind == "NON_NULL" {
			if at(ref, "name") != nil || at(ref, "ofType") == nil {
				t.Errorf("%s: wrapper %v", where, ref)
				return
			}
			checkRef(where, at(ref, "ofType"))
			return
		}
		name, _ := at(ref, "name").(string)
		if types[name] == nil || types[name]["kind"] != kind {
			t.Errorf("%s: reference to %v %q", where, kind, name)
		}
	}
	checkInputs := func(where string, list any) {
		for _, in := range list.([]any) {
			checkRef(where+"."+at(in, "name").(string), at(in, "type"))
		}
	}
	for name, typ := range types {
		kind := typ["kind"].(string)
		for key, kinds := range map[string]string{"fields": "OBJECT INTERFACE", "interfaces": "OBJECT INTERFACE", "possibleTypes": "INTERFACE UNION", "enumValues": "ENUM", "inputFields": "INPUT_OBJECT"} {
			if has := typ[key] != nil; has != contains(strings.Fields(kinds), kind) {
				t.Errorf("%s (%s): %s present = %v", name, kind, key, has)
			}
		}
		if fields, ok := typ["fields"].([]any); ok {
			if !sort.StringsAreSorted(names(fields)) {
				t.Errorf("%s: fields are not sorted: %v", name, names(fields))
			}
			for _, f := range fields {
				where := name + "." + at(f, "name").(string)
				checkRef(where, at(f, "type"))
				checkInputs(where, at(f, "args"))
				if strings.HasPrefix(at(f, "name").(string), "__") {
					t.Errorf("%s is listed", where)
				}
			}
		}
		for _, key := range []string{"interfaces", "possibleTypes"} {
			if list, ok := typ[key].([]any); ok {
				for _, ref := range list {
					checkRef(name+" "+key, ref)
				}
			}
		}
		if list, ok := typ["inputFields"].([]any); ok {
			checkInputs(name, list)
		}
	}
	if t.Failed() {
		return
	}

	// Spot checks of shapes and defaults.
	field := func(typ, name string) any {
		for _, f := range append(append([]any{}, asList(types[typ]["fields"])...), asList(types[typ]["inputFields"])...) {
			if at(f, "name") == name {
				return f
			}
		}
		t.Fatalf("no field %s.%s", typ, name)
		return nil
	}
	issues := field("Repository", "issues")
	if got := at(issues, "type"); !reflect.DeepEqual(got, map[string]any{"kind": "NON_NULL", "name": nil, "ofType": map[string]any{"kind": "OBJECT", "name": "IssueConnection", "ofType": nil}}) {
		t.Errorf("Repository.issues type = %v", got)
	}
	if got := names(at(issues, "args")); !reflect.DeepEqual(got, []string{"after", "before", "filterBy", "first", "labels", "last", "orderBy", "states"}) {
		t.Errorf("Repository.issues args = %v", got)
	}
	states := at(issues, "args", 7, "type")
	if at(states, "kind") != "LIST" || at(states, "ofType", "kind") != "NON_NULL" || at(states, "ofType", "ofType", "name") != "IssueState" {
		t.Errorf("Repository.issues(states:) type = %v", states)
	}
	for where, want := range map[[2]string]any{
		{"MergePullRequestInput", "mergeMethod"}:          "MERGE",
		{"CreatePullRequestInput", "draft"}:               "false",
		{"CreatePullRequestInput", "body"}:                nil,
		{"CreatePullRequestInput", "repositoryId"}:        nil,
		{"CreatePullRequestInput", "maintainerCanModify"}: "true",
	} {
		if got := at(field(where[0], where[1]), "defaultValue"); got != want {
			t.Errorf("%s.%s default = %v, want %v", where[0], where[1], got, want)
		}
	}
	if got := at(field("Query", "repository"), "args", 0); at(got, "name") != "followRenames" || at(got, "defaultValue") != "true" || !strings.Contains(fmt.Sprint(at(got, "description")), "renames") {
		t.Errorf("Query.repository(followRenames:) = %v", got)
	}
	if got := names(types["Node"]["possibleTypes"]); !reflect.DeepEqual(got, []string{"Commit", "Issue", "IssueComment", "Label", "PullRequest", "Ref", "Repository", "User"}) {
		t.Errorf("Node possible types = %v", got)
	}
	if got := names(types["User"]["interfaces"]); !reflect.DeepEqual(got, []string{"Actor", "Node", "RepositoryOwner"}) {
		t.Errorf("User interfaces = %v", got)
	}
	if got := names(types["SearchType"]["enumValues"]); !reflect.DeepEqual(got, []string{"DISCUSSION", "ISSUE", "ISSUE_ADVANCED", "REPOSITORY", "USER"}) {
		t.Errorf("SearchType values = %v", got)
	}
	if got := names(sch["directives"]); !reflect.DeepEqual(got, []string{"deprecated", "include", "skip"}) {
		t.Errorf("directives = %v", got)
	}
	if got := at(sch, "directives", 0, "args", 0); at(got, "defaultValue") != `"No longer supported"` || at(got, "type", "name") != "String" {
		t.Errorf("@deprecated(reason:) = %v", got)
	}
	if got := at(sch, "directives", 1, "locations"); !reflect.DeepEqual(got, []any{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"}) {
		t.Errorf("@include locations = %v", got)
	}
	if types["Repository"]["description"] == nil || at(field("Repository", "issue"), "description") == nil {
		t.Errorf("descriptions are missing")
	}

	// __type: wrappers, unknown names, and that what is introspected can be selected.
	got := h.data(`{ a: __type(name: "Nope") { name } b: __type(name: "IssueOrder") { kind fields { name } inputFields { name type { kind ofType { kind name } } } } c: __type(name: "__Type") { kind name fields { name } } }`, nil)
	if got["a"] != nil || at(got, "b", "kind") != "INPUT_OBJECT" || at(got, "b", "fields") != nil || !reflect.DeepEqual(names(at(got, "b", "inputFields")), []string{"direction", "field"}) || at(got, "b", "inputFields", 0, "type", "ofType", "name") != "OrderDirection" {
		t.Errorf("__type = %v", got)
	}
	if !contains(names(at(got, "c", "fields")), "kind", "name", "fields", "ofType", "enumValues", "inputFields", "interfaces", "possibleTypes") {
		t.Errorf("__Type fields = %v", at(got, "c", "fields"))
	}
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// Every field the schema lists on the data types can be selected and resolves without an error on
// real objects: introspection and execution agree.
func TestEveryFieldResolves(t *testing.T) {
	h := newHarness(t)
	r := seed(t, h)
	s, _ := schema()

	// selection builds a selection of every field of typ, descending into object types to a
	// limited depth and giving required arguments a value.
	var selection func(typ *Type, depth int) string
	selection = func(typ *Type, depth int) string {
		if typ.abstract() {
			out := "{ __typename"
			for _, o := range typ.Possible {
				out += " ... on " + o.Name + " " + selection(o, depth)
			}
			return out + " }"
		}
		out := "{ __typename"
		for _, f := range typ.Fields {
			inner := f.Type.named()
			call := f.Name
			var given []string
			for _, a := range f.Args {
				switch {
				case a.Name == "first":
					given = append(given, "first: 3")
				case a.Name == "number":
					given = append(given, "number: 1")
				case a.Name == "qualifiedName":
					given = append(given, `qualifiedName: "main"`)
				case a.Name == "name" && a.Type.NonNull:
					given = append(given, `name: "bug"`)
				case a.Name == "size":
					given = append(given, "size: 64")
				}
			}
			if len(given) > 0 {
				call += "(" + strings.Join(given, ", ") + ")"
			}
			switch {
			case !inner.composite():
				out += " " + call
			case depth > 0:
				out += " " + call + " " + selection(inner, depth-1)
			}
		}
		return out + " }"
	}
	query := `{ repository(owner: "adaiversen", name: "seeded") ` + selection(s.byName["Repository"], 4) + ` viewer ` + selection(s.byName["User"], 1) + ` }`
	data := h.data(query, nil)
	repo := data["repository"].(map[string]any)
	if repo["__typename"] != "Repository" || repo["databaseId"] != float64(r.ID) || repo["url"] != "http://"+testHost+"/adaiversen/seeded" ||
		repo["createdAt"] != "2026-01-01T00:00:01Z" || repo["visibility"] != "PUBLIC" || repo["description"] != "a seeded repository" {
		t.Errorf("repository = %v", repo)
	}
	if got := at(repo, "issue", "url"); got != "http://"+testHost+"/adaiversen/seeded/issues/1" {
		t.Errorf("issue url = %v", got)
	}
	if got := at(repo, "pullRequest"); got != nil {
		t.Errorf("number 1 is an issue, but pullRequest(number: 1) = %v", got)
	}
	if got := at(repo, "issues", "nodes", 0, "comments", "nodes", 0, "url"); got != "http://"+testHost+"/adaiversen/seeded/issues/1#issuecomment-1" {
		t.Errorf("comment url = %v", got)
	}
	if got := at(data, "viewer", "avatarUrl"); got != "http://"+testHost+"/avatars/u/1000?v=4&s=64" {
		t.Errorf("avatarUrl = %v", got)
	}
}

// seed builds a repository with a little of everything: two labels, four issues (one closed, one
// labelled and commented), branches and three pull requests (open, closed, merged).
func seed(t *testing.T, h *harness) *store.Repo {
	t.Helper()
	r := must[*store.Repo](t)(h.st.CreateRepo(store.NewRepo{Name: "seeded", Description: "a seeded repository", HasDescription: true}))
	must[*store.Label](t)(r.CreateLabel("bug", "d73a4a", "Something is broken", true))
	must[*store.Label](t)(r.CreateLabel("docs", "", "", false))
	must[*store.Issue](t)(r.CreateIssue(store.NewIssue{Title: "alpha", Body: "the first issue, see @adaiversen", Labels: []string{"bug", "docs"}}))
	must[*store.Issue](t)(r.CreateIssue(store.NewIssue{Title: "beta", Body: "the second issue"}))
	must[*store.Issue](t)(r.CreateIssue(store.NewIssue{Title: "gamma", Labels: []string{"docs"}}))
	must[*store.Issue](t)(r.CreateIssue(store.NewIssue{Title: "delta"}))
	must[*store.Comment](t)(r.AddComment(1, "a comment on alpha"))
	must[*store.Comment](t)(r.AddComment(1, "another comment on alpha"))
	closed := "closed"
	if err := r.UpdateIssue(r.Issue(2), store.IssuePatch{State: &closed}); err != nil {
		t.Fatal(err)
	}
	push(t, r, "", "README.md", "initial\n")
	push(t, r, "feature/one", "one.txt", "one\n")
	push(t, r, "feature/two", "dir/two.txt", "two\n")
	push(t, r, "feature/three", "three.txt", "three\n")
	must[*store.Pull](t)(r.CreatePull(store.NewPull{Title: "one", Body: "first pull", Head: "feature/one", Base: "main"})) // #5 open
	two := must[*store.Pull](t)(r.CreatePull(store.NewPull{Title: "two", Head: "feature/two", Base: "main", Draft: true})) // #6 closed
	three := must[*store.Pull](t)(r.CreatePull(store.NewPull{Title: "three", Head: "feature/three", Base: "main"}))        // #7 merged
	must[*store.Comment](t)(r.AddComment(5, "a comment on a pull request"))
	if err := r.UpdatePull(two, store.PullPatch{State: &closed}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.MergePull(three, store.MergeOptions{Method: "squash"}); err != nil {
		t.Fatal(err)
	}
	return r
}

const inSeeded = `repository(owner: "adaiversen", name: "seeded")`

func TestObjects(t *testing.T) {
	h := newHarness(t)
	r := seed(t, h)
	main, _ := r.Branch("main")
	one, _ := r.Branch("feature/one")

	check := func(name, query, want string) {
		t.Helper()
		_, reply := h.send("POST", testToken, requestBody(query, nil))
		if want = `{"data":` + want + `}`; string(reply) != want {
			t.Errorf("%s:\n got %s\nwant %s", name, reply, want)
		}
	}
	check("issue fields", `{ `+inSeeded+` { issue(number: 2) { number title body state closed stateReason databaseId author { login url } repository { nameWithOwner } labels(first: 5) { totalCount } comments(first: 5) { totalCount } closedAt createdAt } } }`,
		`{"repository":{"issue":{"number":2,"title":"beta","body":"the second issue","state":"CLOSED","closed":true,"stateReason":"COMPLETED","databaseId":2,"author":{"login":"adaiversen","url":"http://forge.test:4000/adaiversen"},"repository":{"nameWithOwner":"adaiversen/seeded"},"labels":{"totalCount":0},"comments":{"totalCount":0},"closedAt":"2026-01-01T00:00:10Z","createdAt":"2026-01-01T00:00:05Z"}}}`)
	check("an open issue has no close time or reason, an absent body is empty", `{ `+inSeeded+` { issue(number: 3) { body closedAt stateReason labels { nodes { name color description } } } } }`,
		`{"repository":{"issue":{"body":"","closedAt":null,"stateReason":null,"labels":{"nodes":[{"name":"docs","color":"ededed","description":null}]}}}}`)
	check("issue and pull request numbers do not mix", `{ `+inSeeded+` { a: issue(number: 5) { id } b: pullRequest(number: 1) { id } c: issue(number: 99) { id } d: pullRequest(number: 0) { id } } }`,
		`{"repository":{"a":null,"b":null,"c":null,"d":null}}`)
	check("pull request fields", `{ `+inSeeded+` { pullRequest(number: 5) { number title body state merged closed isDraft mergeable headRefName baseRefName headRefOid baseRefOid additions deletions changedFiles mergedAt url author { login } headRef { name prefix } baseRef { name target { oid } } comments(first: 1) { nodes { body } } files { totalCount nodes { path additions deletions changeType } } } } }`,
		`{"repository":{"pullRequest":{"number":5,"title":"one","body":"first pull","state":"OPEN","merged":false,"closed":false,"isDraft":false,"mergeable":"MERGEABLE","headRefName":"feature/one","baseRefName":"main","headRefOid":"`+one.String()+`","baseRefOid":"`+main.String()+`","additions":1,"deletions":0,"changedFiles":1,"mergedAt":null,"url":"http://forge.test:4000/adaiversen/seeded/pull/5","author":{"login":"adaiversen"},"headRef":{"name":"feature/one","prefix":"refs/heads/"},"baseRef":{"name":"main","target":{"oid":"`+main.String()+`"}},"comments":{"nodes":[{"body":"a comment on a pull request"}]},"files":{"totalCount":1,"nodes":[{"path":"one.txt","additions":1,"deletions":0,"changeType":"ADDED"}]}}}}`)
	check("closed and merged pull requests", `{ `+inSeeded+` { a: pullRequest(number: 6) { state merged closed isDraft mergeable files { nodes { path } } } b: pullRequest(number: 7) { state merged closed mergeable mergedAt files { nodes { path } } } } }`,
		`{"repository":{"a":{"state":"CLOSED","merged":false,"closed":true,"isDraft":true,"mergeable":"UNKNOWN","files":{"nodes":[{"path":"dir/two.txt"}]}},"b":{"state":"MERGED","merged":true,"closed":true,"mergeable":"UNKNOWN","mergedAt":"`+stamp(r.Pull(7).MergedAt).(string)+`","files":{"nodes":[{"path":"three.txt"}]}}}}`)
	check("pull request filters", `{ `+inSeeded+` { all: pullRequests(first: 10) { totalCount nodes { number } } open: pullRequests(states: OPEN) { nodes { number } } closed: pullRequests(states: [CLOSED]) { nodes { number } } merged: pullRequests(states: [MERGED, CLOSED], orderBy: {field: CREATED_AT, direction: DESC}) { nodes { number } } head: pullRequests(headRefName: "feature/two", baseRefName: "main") { nodes { number } } none: pullRequests(baseRefName: "nope") { totalCount } labelled: pullRequests(labels: ["bug"]) { totalCount } } }`,
		`{"repository":{"all":{"totalCount":3,"nodes":[{"number":5},{"number":6},{"number":7}]},"open":{"nodes":[{"number":5}]},"closed":{"nodes":[{"number":6}]},"merged":{"nodes":[{"number":7},{"number":6}]},"head":{"nodes":[{"number":6}]},"none":{"totalCount":0},"labelled":{"totalCount":0}}}`)
	check("issue filters", `{ `+inSeeded+` { all: issues(first: 10) { totalCount nodes { number } } bug: issues(labels: ["BUG"]) { nodes { number } } both: issues(labels: ["bug", "docs"]) { nodes { number } } closed: issues(filterBy: {states: [CLOSED]}) { nodes { number } } docs: issues(filterBy: {labels: ["docs"]}, states: [OPEN, CLOSED]) { nodes { number } } mine: issues(filterBy: {createdBy: "ADAIVERSEN"}) { totalCount } theirs: issues(filterBy: {createdBy: "someone"}) { totalCount } assigned: issues(filterBy: {assignee: "adaiversen"}) { totalCount } mentioned: issues(filterBy: {mentioned: "adaiversen"}) { nodes { number } } recent: issues(filterBy: {since: "2026-01-01T00:00:09Z"}) { nodes { number } } } }`,
		`{"repository":{"all":{"totalCount":4,"nodes":[{"number":1},{"number":2},{"number":3},{"number":4}]},"bug":{"nodes":[{"number":1}]},"both":{"nodes":[{"number":1}]},"closed":{"nodes":[{"number":2}]},"docs":{"nodes":[{"number":1},{"number":3}]},"mine":{"totalCount":4},"theirs":{"totalCount":0},"assigned":{"totalCount":0},"mentioned":{"nodes":[{"number":1}]},"recent":{"nodes":[{"number":1},{"number":2}]}}}`)
	check("issue orders", `{ `+inSeeded+` { updated: issues(orderBy: {field: UPDATED_AT, direction: DESC}) { nodes { number } } comments: issues(orderBy: {field: COMMENTS, direction: DESC}) { nodes { number } } commentsAsc: issues(orderBy: {field: COMMENTS, direction: ASC}) { nodes { number } } } }`,
		`{"repository":{"updated":{"nodes":[{"number":2},{"number":1},{"number":4},{"number":3}]},"comments":{"nodes":[{"number":1},{"number":4},{"number":3},{"number":2}]},"commentsAsc":{"nodes":[{"number":2},{"number":3},{"number":4},{"number":1}]}}}`)
	check("labels", `{ `+inSeeded+` { label(name: "BUG") { id name color description } missing: label(name: "nope") { id } labels(first: 1) { totalCount nodes { name } pageInfo { hasNextPage } } } }`,
		`{"repository":{"label":{"id":"`+store.NodeID(store.KindLabel, 1)+`","name":"bug","color":"d73a4a","description":"Something is broken"},"missing":null,"labels":{"totalCount":2,"nodes":[{"name":"bug"}],"pageInfo":{"hasNextPage":true}}}}`)
	check("refs and commits", `{ `+inSeeded+` { isEmpty defaultBranchRef { name prefix target { oid message } } a: ref(qualifiedName: "refs/heads/feature/one") { name prefix } b: ref(qualifiedName: "feature/one") { name } c: ref(qualifiedName: "heads/main") { name } d: ref(qualifiedName: "nope") { name } } }`,
		`{"repository":{"isEmpty":false,"defaultBranchRef":{"name":"main","prefix":"refs/heads/","target":{"oid":"`+main.String()+`","message":"three (#7)"}},"a":{"name":"feature/one","prefix":"refs/heads/"},"b":{"name":"feature/one"},"c":{"name":"main"},"d":null}}`)
	check("users", `{ viewer { id databaseId login name email url avatarUrl } user(login: "AdaIversen") { login } repositoryOwner(login: "adaiversen") { __typename login id ... on User { name } } nobody: repositoryOwner(login: "nobody") { login } }`,
		`{"viewer":{"id":"`+store.ViewerNodeID+`","databaseId":1000,"login":"adaiversen","name":"ada iversen","email":"adaiversen@example.invalid","url":"http://forge.test:4000/adaiversen","avatarUrl":"http://forge.test:4000/avatars/u/1000?v=4"},"user":{"login":"adaiversen"},"repositoryOwner":{"__typename":"User","login":"adaiversen","id":"`+store.ViewerNodeID+`","name":"ada iversen"},"nobody":null}`)
	check("the repository is found whatever the case of its owner and name", `{ repository(owner: "AdaIversen", name: "SEEDED") { name } other: repository(owner: "someone", name: "seeded") { name } }`,
		`{"repository":{"name":"seeded"},"other":null}`)

	// Every kind of node round-trips through node(id:) with its own type and id.
	ids := h.data(`{ viewer { id } `+inSeeded+` { id issue(number: 1) { id comments(first: 1) { nodes { id } } } pullRequest(number: 5) { id } label(name: "bug") { id } defaultBranchRef { id target { id } } } }`, nil)
	wantTypes := map[string]string{
		at(ids, "viewer", "id").(string):                                      "User",
		at(ids, "repository", "id").(string):                                  "Repository",
		at(ids, "repository", "issue", "id").(string):                         "Issue",
		at(ids, "repository", "issue", "comments", "nodes", 0, "id").(string): "IssueComment",
		at(ids, "repository", "pullRequest", "id").(string):                   "PullRequest",
		at(ids, "repository", "label", "id").(string):                         "Label",
		at(ids, "repository", "defaultBranchRef", "id").(string):              "Ref",
		at(ids, "repository", "defaultBranchRef", "target", "id").(string):    "Commit",
	}
	if len(wantTypes) != 8 {
		t.Fatalf("node ids are not distinct: %v", ids)
	}
	var all []string
	for id := range wantTypes {
		all = append(all, id)
	}
	sort.Strings(all)
	nodes := h.data(`query($ids: [ID!]!) { nodes(ids: $ids) { __typename id } }`, map[string]any{"ids": all})["nodes"].([]any)
	for i, id := range all {
		if at(nodes, i, "id") != id || at(nodes, i, "__typename") != wantTypes[id] {
			t.Errorf("node(%s) = %v, want a %s", id, nodes[i], wantTypes[id])
		}
		single := h.data(`query($id: ID!) { node(id: $id) { __typename id } }`, map[string]any{"id": id})
		if !reflect.DeepEqual(single["node"], nodes[i]) {
			t.Errorf("node(%s) = %v, nodes gave %v", id, single["node"], nodes[i])
		}
	}
	// REST's node_id is the same string (the store's), so the surfaces agree on identity.
	if at(ids, "repository", "issue", "id") != store.NodeID(store.KindIssue, r.Issue(1).ID) || at(ids, "repository", "pullRequest", "id") != store.NodeID(store.KindPull, r.Pull(5).ID) {
		t.Errorf("ids differ from the store's node ids: %v", ids)
	}
}

func TestPagination(t *testing.T) {
	h := newHarness(t)
	r := must[*store.Repo](t)(h.st.CreateRepo(store.NewRepo{Name: "pages"}))
	for i := 1; i <= 7; i++ {
		must[*store.Issue](t)(r.CreateIssue(store.NewIssue{Title: "issue " + strconv.Itoa(i)}))
	}
	const doc = `query($first: Int, $after: String, $last: Int, $before: String, $states: [IssueState!], $order: IssueOrder) {
  repository(owner: "adaiversen", name: "pages") {
    issues(first: $first, after: $after, last: $last, before: $before, states: $states, orderBy: $order) {
      totalCount
      nodes { number }
      edges { cursor node { number } }
      pageInfo { hasNextPage hasPreviousPage startCursor endCursor }
    }
  }
}`
	page := func(vars map[string]any) (nums []int, info map[string]any, conn any) {
		t.Helper()
		conn = at(h.data(doc, vars), "repository", "issues")
		edges := at(conn, "edges").([]any)
		nums = numbers(at(conn, "nodes"))
		info = at(conn, "pageInfo").(map[string]any)
		// Edges mirror nodes, and the page's cursors are those of its first and last edge.
		for i, e := range edges {
			if int(at(e, "node", "number").(float64)) != nums[i] {
				t.Errorf("edges and nodes differ: %v", conn)
			}
		}
		if len(edges) == 0 {
			if info["startCursor"] != nil || info["endCursor"] != nil {
				t.Errorf("an empty page has cursors: %v", info)
			}
		} else if info["startCursor"] != at(edges, 0, "cursor") || info["endCursor"] != at(edges, len(edges)-1, "cursor") {
			t.Errorf("page cursors are not the edge cursors: %v", conn)
		}
		return nums, info, conn
	}
	expect := func(name string, vars map[string]any, want []int, next, prev bool) map[string]any {
		t.Helper()
		nums, info, conn := page(vars)
		if nums == nil {
			nums = []int{}
		}
		if !reflect.DeepEqual(nums, want) || info["hasNextPage"] != next || info["hasPreviousPage"] != prev || at(conn, "totalCount") == nil {
			t.Errorf("%s: %v next=%v prev=%v, want %v next=%v prev=%v", name, nums, info["hasNextPage"], info["hasPreviousPage"], want, next, prev)
		}
		return info
	}

	first := expect("first page", map[string]any{"first": 3}, []int{1, 2, 3}, true, false)
	second := expect("second page", map[string]any{"first": 3, "after": first["endCursor"]}, []int{4, 5, 6}, true, true)
	expect("last page", map[string]any{"first": 3, "after": second["endCursor"]}, []int{7}, false, true)
	expect("the same cursor again", map[string]any{"first": 3, "after": first["endCursor"]}, []int{4, 5, 6}, true, true)
	tail := expect("from the end", map[string]any{"last": 2}, []int{6, 7}, false, true)
	expect("backwards", map[string]any{"last": 2, "before": tail["startCursor"]}, []int{4, 5}, true, true)
	expect("between two cursors", map[string]any{"first": 10, "after": first["endCursor"], "before": tail["startCursor"]}, []int{4, 5}, true, true)
	expect("first and last together", map[string]any{"first": 5, "last": 2}, []int{4, 5}, true, true)
	expect("zero", map[string]any{"first": 0}, []int{}, true, false)
	expect("negative", map[string]any{"first": -5}, []int{}, true, false)
	expect("more than there is", map[string]any{"first": 100}, []int{1, 2, 3, 4, 5, 6, 7}, false, false)
	expect("no size at all", nil, []int{1, 2, 3, 4, 5, 6, 7}, false, false)
	expect("after the end", map[string]any{"first": 3, "after": tail["endCursor"]}, []int{}, false, true)
	desc := map[string]any{"field": "CREATED_AT", "direction": "DESC"}
	top := expect("descending", map[string]any{"first": 3, "order": desc}, []int{7, 6, 5}, true, false)
	expect("descending, next", map[string]any{"first": 3, "order": desc, "after": top["endCursor"]}, []int{4, 3, 2}, true, true)

	// A cursor keeps its place when its issue leaves the filtered list, and when issues arrive.
	open := []string{"OPEN"}
	firstOpen := expect("open issues", map[string]any{"first": 3, "states": open}, []int{1, 2, 3}, true, false)
	closed := "closed"
	if err := r.UpdateIssue(r.Issue(3), store.IssuePatch{State: &closed}); err != nil {
		t.Fatal(err)
	}
	must[*store.Issue](t)(r.CreateIssue(store.NewIssue{Title: "issue 8"}))
	expect("open issues after #3 closed", map[string]any{"first": 3, "states": open, "after": firstOpen["endCursor"]}, []int{4, 5, 6}, true, true)
	expect("descending after #8 arrived", map[string]any{"first": 3, "order": desc, "after": top["endCursor"]}, []int{4, 3, 2}, true, true)

	// The page size is capped; totalCount is not.
	big := must[*store.Repo](t)(h.st.CreateRepo(store.NewRepo{Name: "big"}))
	for i := 0; i < 130; i++ {
		must[*store.Issue](t)(big.CreateIssue(store.NewIssue{Title: "n"}))
	}
	conn := at(h.data(`{ repository(owner: "adaiversen", name: "big") { issues(first: 1000) { totalCount nodes { number } pageInfo { hasNextPage } } unbounded: issues { nodes { number } } } }`, nil), "repository")
	if got := numbers(at(conn, "issues", "nodes")); len(got) != 100 || at(conn, "issues", "totalCount") != 130.0 || at(conn, "issues", "pageInfo", "hasNextPage") != true || len(numbers(at(conn, "unbounded", "nodes"))) != 100 {
		t.Errorf("capped page: %d nodes, %v", len(got), at(conn, "issues", "pageInfo"))
	}

	// Cursors of one listing mean nothing to another.
	foreign := h.post(`query($c: String) { repository(owner: "adaiversen", name: "big") { labels(after: $c) { totalCount } } }`, map[string]any{"c": first["endCursor"]})
	if len(foreign.errors) != 1 || at(foreign.errors, 0, "type") != "INVALID_CURSOR_ARGUMENTS" || at(foreign.data, "repository", "labels") != nil {
		t.Errorf("foreign cursor: %s", foreign.raw)
	}
}

func TestMutations(t *testing.T) {
	h := newHarness(t)
	r := seed(t, h)
	vars := map[string]any{
		"repo": r.NodeID, "issue": store.NodeID(store.KindIssue, r.Issue(4).ID), "pull": store.NodeID(store.KindPull, r.Pull(5).ID),
		"labels": []string{store.NodeID(store.KindLabel, 2), "LA_nope", store.NodeID(store.KindLabel, 2)},
	}
	got := h.data(`mutation($repo: ID!, $issue: ID!, $pull: ID!, $labels: [ID!]) {
  createIssue(input: {repositoryId: $repo, title: "made here", labelIds: $labels, assigneeIds: [], clientMutationId: "abc"}) {
    clientMutationId
    issue { number title body state labels(first: 5) { nodes { name } } }
  }
  notPlanned: closeIssue(input: {issueId: $issue, stateReason: NOT_PLANNED}) { issue { state stateReason closed } }
  again: closeIssue(input: {issueId: $issue}) { issue { state stateReason } }
  reopenIssue(input: {issueId: $issue}) { issue { state stateReason closedAt } }
  onPull: addComment(input: {subjectId: $pull, body: "on the pull request"}) {
    commentEdge { cursor node { body databaseId url } }
    subject { __typename ... on PullRequest { number comments { totalCount } } }
  }
  closePullRequest(input: {pullRequestId: $pull}) { pullRequest { state closed merged } }
}`, vars)
	want := `{"createIssue":{"clientMutationId":"abc","issue":{"number":8,"title":"made here","body":"","state":"OPEN","labels":{"nodes":[{"name":"docs"}]}}},` +
		`"notPlanned":{"issue":{"state":"CLOSED","stateReason":"NOT_PLANNED","closed":true}},` +
		`"again":{"issue":{"state":"CLOSED","stateReason":"NOT_PLANNED"}},` +
		`"reopenIssue":{"issue":{"state":"OPEN","stateReason":"REOPENED","closedAt":null}},` +
		`"onPull":{"commentEdge":{"cursor":"` + cursorOf("4") + `","node":{"body":"on the pull request","databaseId":4,"url":"http://forge.test:4000/adaiversen/seeded/issues/5#issuecomment-4"}},"subject":{"__typename":"PullRequest","number":5,"comments":{"totalCount":2}}},` +
		`"closePullRequest":{"pullRequest":{"state":"CLOSED","closed":true,"merged":false}}}`
	if raw, _ := json.Marshal(got); !jsonEqual(string(raw), want) {
		t.Errorf("mutations:\n got %s\nwant %s", raw, want)
	}
	// The store, which REST reads, has all of it.
	if is := r.Issue(8); is == nil || is.Title != "made here" || len(is.Labels) != 1 || r.Issue(4).Closed || !r.Pull(5).Closed || len(r.Pull(5).Comments) != 2 {
		t.Errorf("the store does not reflect the mutations")
	}
	// A full ref name is accepted for the branches of a new pull request.
	push(t, r, "feature/four", "four.txt", "four\n")
	pr := h.data(`mutation($repo: ID!) { createPullRequest(input: {repositoryId: $repo, title: "four", body: "b", headRefName: "refs/heads/feature/four", baseRefName: "refs/heads/main", draft: true}) { pullRequest { number isDraft body headRefName baseRefName } } }`, vars)
	if raw, _ := json.Marshal(pr); string(raw) != `{"createPullRequest":{"pullRequest":{"baseRefName":"main","body":"b","headRefName":"feature/four","isDraft":true,"number":9}}}` {
		t.Errorf("createPullRequest = %s", raw)
	}
	// A rebase merge with a headline; a head that moved is refused.
	stale := h.post(`mutation($id: ID!) { mergePullRequest(input: {pullRequestId: $id, expectedHeadOid: "0000000000000000000000000000000000000001"}) { pullRequest { merged } } }`, map[string]any{"id": store.NodeID(store.KindPull, r.Pull(9).ID)})
	if len(stale.errors) != 1 || at(stale.errors, 0, "type") != "UNPROCESSABLE" || at(stale.errors, 0, "message") != store.MsgHeadModified || r.Pull(9).Merged {
		t.Errorf("stale head: %s", stale.raw)
	}
	h.data(`mutation($id: ID!) { mergePullRequest(input: {pullRequestId: $id, mergeMethod: REBASE, commitHeadline: "headline", commitBody: "body"}) { pullRequest { merged } } }`, map[string]any{"id": store.NodeID(store.KindPull, r.Pull(9).ID)})
	tip, _ := r.Branch("main")
	if msg := at(h.data(`{ `+inSeeded+` { defaultBranchRef { target { message oid } } } }`, nil), "repository", "defaultBranchRef", "target"); at(msg, "message") != "headline\n\nbody" || at(msg, "oid") != tip.String() {
		t.Errorf("merge commit = %v", msg)
	}
}

// jsonEqual compares two JSON texts as values.
func jsonEqual(a, b string) bool {
	var x, y any
	return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal([]byte(b), &y) == nil && reflect.DeepEqual(x, y)
}

func TestSearch(t *testing.T) {
	h := newHarness(t)
	seed(t, h)
	other := must[*store.Repo](t)(h.st.CreateRepo(store.NewRepo{Name: "other", Description: "second one", HasDescription: true, Private: true}))
	must[*store.Issue](t)(other.CreateIssue(store.NewIssue{Title: "alpha too", Body: "in the other repository"}))

	const doc = `query($q: String!, $type: SearchType!, $first: Int, $after: String) {
  search(query: $q, type: $type, first: $first, after: $after) {
    issueCount repositoryCount userCount
    nodes { __typename ... on Issue { number repository { name } } ... on PullRequest { number } ... on Repository { name } ... on User { login } }
    pageInfo { hasNextPage endCursor }
  }
}`
	found := func(q, kind string) string {
		t.Helper()
		var out []string
		for _, n := range at(h.data(doc, map[string]any{"q": q, "type": kind, "first": 50}), "search", "nodes").([]any) {
			switch at(n, "__typename") {
			case "Issue":
				out = append(out, fmt.Sprintf("%v#%v", at(n, "repository", "name"), at(n, "number")))
			case "PullRequest":
				out = append(out, fmt.Sprintf("pr#%v", at(n, "number")))
			case "Repository":
				out = append(out, at(n, "name").(string))
			case "User":
				out = append(out, "@"+at(n, "login").(string))
			}
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct{ q, kind, want string }{
		{"alpha", "ISSUE", "other#1 seeded#1"},
		{"alpha repo:adaiversen/seeded", "ISSUE", "seeded#1"},
		{"ALPHA sort:created-asc", "ISSUE_ADVANCED", "seeded#1 other#1"},
		{`"second issue"`, "ISSUE", "seeded#2"},
		{"second issue first", "ISSUE", ""},
		{"is:pr repo:adaiversen/seeded", "ISSUE", "pr#7 pr#6 pr#5"},
		{"is:pr is:merged", "ISSUE", "pr#7"},
		{"is:pr is:open", "ISSUE", "pr#5"},
		{"is:issue is:closed", "ISSUE", "seeded#2"},
		{"type:issue state:open repo:adaiversen/seeded", "ISSUE", "seeded#4 seeded#3 seeded#1"},
		{"label:docs label:bug", "ISSUE", "seeded#1"},
		{"comment in:comments", "ISSUE", "pr#5 seeded#1"},
		{"alpha in:body", "ISSUE", ""},
		{"alpha user:adaiversen", "ISSUE", "other#1 seeded#1"},
		{"alpha author:@me", "ISSUE", "other#1 seeded#1"},
		{"alpha org:someone", "ISSUE", ""},
		{"is:issue sort:comments-desc repo:adaiversen/seeded", "ISSUE", "seeded#1 seeded#4 seeded#3 seeded#2"},
		{"", "REPOSITORY", "seeded other"},
		{"second", "REPOSITORY", "other"},
		{"second in:name", "REPOSITORY", ""},
		{"is:private", "REPOSITORY", "other"},
		{"is:public", "REPOSITORY", "seeded"},
		{"ada", "USER", "@adaiversen"},
		{"nobody", "USER", ""},
		{"alpha", "DISCUSSION", ""},
	} {
		if got := found(c.q, c.kind); got != c.want {
			t.Errorf("search %q (%s) = %q, want %q", c.q, c.kind, got, c.want)
		}
	}
	// Counts cover the whole result, pages cut it.
	s := at(h.data(doc, map[string]any{"q": "", "type": "ISSUE", "first": 2}), "search")
	if at(s, "issueCount") != 8.0 || at(s, "repositoryCount") != 0.0 || len(at(s, "nodes").([]any)) != 2 || at(s, "pageInfo", "hasNextPage") != true {
		t.Errorf("search page 1 = %v", s)
	}
	s = at(h.data(doc, map[string]any{"q": "", "type": "ISSUE", "first": 50, "after": at(s, "pageInfo", "endCursor")}), "search")
	if len(at(s, "nodes").([]any)) != 6 || at(s, "pageInfo", "hasNextPage") != false {
		t.Errorf("search page 2 = %v", s)
	}
}
