package graphql

// Introspection: the resolvers of introspection.graphql. They read the schema registry itself.
// The Go values behind the introspection types are:
//
//	__Schema      *Schema          __Type        *TypeRef (a named type, a list or a non-null)
//	__Field       *FieldDef        __InputValue  *InputDef
//	__EnumValue   *EnumValue       __Directive   *DirectiveDef

// plain returns the named type t stands for, or nil when t is a LIST or NON_NULL wrapper.
func plain(t *TypeRef) *Type {
	if t.NonNull || t.Elem != nil {
		return nil
	}
	return t.Named
}

func typeRefs(types []*Type) []any {
	out := make([]any, len(types))
	for i, t := range types {
		out[i] = t.ref
	}
	return out
}

func introspection(s *Schema) {
	call(s, metaType, callers[root]{
		"__schema": func(q *query, _ root, _ args) (any, error) { return q.s, nil },
		"__type": func(q *query, _ root, a args) (any, error) {
			if t := q.s.byName[a.str("name")]; t != nil {
				return t.ref, nil
			}
			return nil, nil
		},
	})

	get(s, "__Schema", getters[*Schema]{
		"description": func(*query, *Schema) any { return nil },
		"types":       func(_ *query, s *Schema) any { return typeRefs(s.Types) },
		"queryType":   func(_ *query, s *Schema) any { return s.Query.ref },
		"mutationType": func(_ *query, s *Schema) any {
			if s.Mutation == nil {
				return nil
			}
			return s.Mutation.ref
		},
		"subscriptionType": func(*query, *Schema) any { return nil },
		"directives":       func(_ *query, s *Schema) any { return items(s.Directives) },
	})

	get(s, "__Type", getters[*TypeRef]{
		"kind": func(_ *query, t *TypeRef) any {
			switch {
			case t.NonNull:
				return "NON_NULL"
			case t.Elem != nil:
				return "LIST"
			}
			return kindNames[t.Named.Kind]
		},
		"name": func(_ *query, t *TypeRef) any {
			if n := plain(t); n != nil {
				return n.Name
			}
			return nil
		},
		"description": func(_ *query, t *TypeRef) any {
			if n := plain(t); n != nil {
				return text(n.Description)
			}
			return nil
		},
		"specifiedByURL": func(*query, *TypeRef) any { return nil },
		"interfaces": func(_ *query, t *TypeRef) any {
			if n := plain(t); n != nil && (n.Kind == Object || n.Kind == Interface) {
				return typeRefs(n.Interfaces)
			}
			return nil
		},
		"possibleTypes": func(_ *query, t *TypeRef) any {
			if n := plain(t); n != nil && n.abstract() {
				return typeRefs(n.Possible)
			}
			return nil
		},
		"ofType": func(_ *query, t *TypeRef) any {
			switch {
			case t.NonNull && t.Elem != nil:
				return &TypeRef{Elem: t.Elem}
			case t.NonNull:
				return t.Named.ref
			case t.Elem != nil:
				return t.Elem
			}
			return nil
		},
	})
	call(s, "__Type", callers[*TypeRef]{
		"fields": func(_ *query, t *TypeRef, a args) (any, error) {
			n := plain(t)
			if n == nil || (n.Kind != Object && n.Kind != Interface) {
				return nil, nil
			}
			out := make([]any, 0, len(n.Fields))
			for _, f := range n.Fields {
				if !f.Deprecated || a.flag("includeDeprecated") {
					out = append(out, f)
				}
			}
			return out, nil
		},
		"enumValues": func(_ *query, t *TypeRef, a args) (any, error) {
			n := plain(t)
			if n == nil || n.Kind != Enum {
				return nil, nil
			}
			out := make([]any, 0, len(n.Values))
			for _, v := range n.Values {
				if !v.Deprecated || a.flag("includeDeprecated") {
					out = append(out, v)
				}
			}
			return out, nil
		},
		"inputFields": func(_ *query, t *TypeRef, _ args) (any, error) {
			if n := plain(t); n != nil && n.Kind == InputObject {
				return items(n.Inputs), nil
			}
			return nil, nil
		},
	})

	get(s, "__Field", getters[*FieldDef]{
		"name":         func(_ *query, f *FieldDef) any { return f.Name },
		"description":  func(_ *query, f *FieldDef) any { return text(f.Description) },
		"type":         func(_ *query, f *FieldDef) any { return f.Type },
		"isDeprecated": func(_ *query, f *FieldDef) any { return f.Deprecated },
		"deprecationReason": func(_ *query, f *FieldDef) any {
			if !f.Deprecated {
				return nil
			}
			return f.Reason
		},
	})
	call(s, "__Field", callers[*FieldDef]{
		"args": func(_ *query, f *FieldDef, _ args) (any, error) { return items(f.Args), nil },
	})

	get(s, "__InputValue", getters[*InputDef]{
		"name":        func(_ *query, in *InputDef) any { return in.Name },
		"description": func(_ *query, in *InputDef) any { return text(in.Description) },
		"type":        func(_ *query, in *InputDef) any { return in.Type },
		"defaultValue": func(_ *query, in *InputDef) any {
			if in.Default == nil {
				return nil
			}
			return in.Default.String()
		},
		"isDeprecated":      func(*query, *InputDef) any { return false },
		"deprecationReason": func(*query, *InputDef) any { return nil },
	})

	get(s, "__EnumValue", getters[*EnumValue]{
		"name":         func(_ *query, v *EnumValue) any { return v.Name },
		"description":  func(_ *query, v *EnumValue) any { return text(v.Description) },
		"isDeprecated": func(_ *query, v *EnumValue) any { return v.Deprecated },
		"deprecationReason": func(_ *query, v *EnumValue) any {
			if !v.Deprecated {
				return nil
			}
			return v.Reason
		},
	})

	get(s, "__Directive", getters[*DirectiveDef]{
		"name":         func(_ *query, d *DirectiveDef) any { return d.Name },
		"description":  func(_ *query, d *DirectiveDef) any { return text(d.Description) },
		"locations":    func(_ *query, d *DirectiveDef) any { return items(d.Locations) },
		"isRepeatable": func(_ *query, d *DirectiveDef) any { return d.Repeatable },
	})
	call(s, "__Directive", callers[*DirectiveDef]{
		"args": func(_ *query, d *DirectiveDef, _ args) (any, error) { return items(d.Args), nil },
	})
}
