package gitobj

// Merge3 merges the trees ours and theirs relative to their common ancestor base, path by path:
// a path only one side changed takes that side's version; a path both sides changed the same way
// takes it; a path the two sides changed differently is a conflict (ok = false). A file on one
// side against a directory on the other is a conflict too. There is no line-level merging.
//
// Any id may be Zero, meaning "no tree". With write = false nothing is stored and only ok is
// meaningful, which makes the same function the mergeability test. With write = true the merged
// tree is stored (the empty tree when nothing is left) and its id returned.
func (db *DB) Merge3(base, ours, theirs OID, write bool) (merged OID, ok bool) {
	id, ok := db.merge3(norm(base), norm(ours), norm(theirs), write)
	if !ok {
		return Zero, false
	}
	if write && id.IsZero() {
		id = db.PutTree(nil)
	}
	return id, true
}

// norm maps the empty tree to Zero so "absent" has one representation inside the merge.
func norm(id OID) OID {
	if id == EmptyTree {
		return Zero
	}
	return id
}

// merge3 returns Zero for an empty result.
func (db *DB) merge3(base, ours, theirs OID, write bool) (OID, bool) {
	switch {
	case theirs == base:
		return ours, true
	case ours == base, ours == theirs:
		return theirs, true
	}
	eb, err1 := db.Tree(base)
	eo, err2 := db.Tree(ours)
	et, err3 := db.Tree(theirs)
	if err1 != nil || err2 != nil || err3 != nil {
		return Zero, false
	}
	// Visit the union of names: ours first, then names only theirs or only base have.
	var out []Entry
	visit := func(name string) bool {
		m, o, t := pick(eb, name), pick(eo, name), pick(et, name)
		var res Entry
		switch {
		case sameEntry(t, m):
			res = o
		case sameEntry(o, m), sameEntry(o, t):
			res = t
		case o.IsDir() && t.IsDir():
			sub := Zero
			if m.IsDir() {
				sub = m.ID
			}
			id, ok := db.merge3(sub, o.ID, t.ID, write)
			if !ok {
				return false
			}
			if !id.IsZero() {
				res = Entry{Mode: ModeDir, Name: name, ID: id}
			}
		default:
			return false
		}
		if res.Mode != 0 {
			out = append(out, res)
		}
		return true
	}
	for i := range eo {
		if !visit(eo[i].Name) {
			return Zero, false
		}
	}
	for i := range et {
		if indexEntry(eo, et[i].Name) < 0 && !visit(et[i].Name) {
			return Zero, false
		}
	}
	for i := range eb {
		if indexEntry(eo, eb[i].Name) < 0 && indexEntry(et, eb[i].Name) < 0 && !visit(eb[i].Name) {
			return Zero, false
		}
	}
	if len(out) == 0 {
		return Zero, true
	}
	if !write {
		return notEmpty, true
	}
	return db.PutTree(out), true
}

// notEmpty stands in for "some non-empty tree" while only testing mergeability; it is never stored.
var notEmpty = OID{0xff}

// pick returns the entry called name, or the zero Entry (Mode 0) when the tree has none.
func pick(entries []Entry, name string) Entry {
	if i := indexEntry(entries, name); i >= 0 {
		return entries[i]
	}
	return Entry{}
}

func sameEntry(a, b Entry) bool { return a.Mode == b.Mode && a.ID == b.ID }
