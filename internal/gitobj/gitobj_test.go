package gitobj

import (
	"bytes"
	"compress/zlib"
	"io"
	"testing"
)

// The expected ids in this file were produced by git itself (hash-object, mktree) for the same
// content, so they pin our encodings to git's.

func newDB() *DB { return NewDB(&Arena{}) }

func TestObjectIDs(t *testing.T) {
	db := newDB()
	if got := db.Put(TypeBlob, []byte("hello\n")).ID.String(); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("blob id = %s", got)
	}
	if got := db.PutTree(nil); got != EmptyTree {
		t.Fatalf("empty tree id = %s", got)
	}
	if db.Put(TypeBlob, []byte("hello\n")) != db.Put(TypeBlob, []byte("hello\n")) {
		t.Fatal("storing the same content twice must return the same object")
	}
}

func TestTreeOrderAndPaths(t *testing.T) {
	db := newDB()
	x := db.Put(TypeBlob, []byte("x")).ID
	root := Zero
	var err error
	// Insert in an order that is not git's: the directory "a" must sort after "a-b" and "a.txt".
	for _, f := range []struct {
		path string
		mode uint32
	}{{"b", ModeExec}, {"a/f", ModeFile}, {"a.txt", ModeFile}, {"a-b", ModeFile}} {
		if root, err = db.SetPath(root, f.path, f.mode, x); err != nil {
			t.Fatal(err)
		}
	}
	if got := root.String(); got != "a02ba09c1bb9045c86b7d2a584e20eb29d3955f5" {
		t.Fatalf("tree id = %s", got)
	}
	if e, ok := db.Find(root, "a/f"); !ok || e.ID != x || e.Mode != ModeFile {
		t.Fatalf("Find a/f = %+v %v", e, ok)
	}
	if e, ok := db.Find(root, "a"); !ok || !e.IsDir() || e.ID.String() != "2561a62d4223eb7660d3b6b02b707048382f4019" {
		t.Fatalf("Find a = %+v %v", e, ok)
	}
	if _, ok := db.Find(root, "a/missing"); ok {
		t.Fatal("found a path that does not exist")
	}
	if _, ok := db.Find(root, "b/under-a-file"); ok {
		t.Fatal("found a path below a file")
	}
	if _, err := db.SetPath(root, "b/under-a-file", ModeFile, x); err != ErrNotDir {
		t.Fatalf("SetPath through a file: %v", err)
	}

	// Removing a/f prunes the now empty directory; removing everything leaves the empty tree.
	root, err = db.RemovePath(root, "a/f")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Find(root, "a"); ok {
		t.Fatal("empty directory was not pruned")
	}
	for _, p := range []string{"a-b", "a.txt", "b"} {
		if root, err = db.RemovePath(root, p); err != nil {
			t.Fatal(err)
		}
	}
	if root != EmptyTree || !db.Has(EmptyTree) {
		t.Fatalf("root after removing everything = %s", root)
	}
	if _, err := db.RemovePath(root, "nope"); err != ErrNoEntry {
		t.Fatalf("RemovePath missing: %v", err)
	}
}

func TestCommitEncoding(t *testing.T) {
	db := newDB()
	x := db.Put(TypeBlob, []byte("x")).ID
	root := Zero
	for _, f := range []struct {
		path string
		mode uint32
	}{{"a-b", ModeFile}, {"a.txt", ModeFile}, {"a/f", ModeFile}, {"b", ModeExec}} {
		root, _ = db.SetPath(root, f.path, f.mode, x)
	}
	who := Signature{Name: "ada iversen", Email: "adaiversen@example.invalid", When: 1767225601}
	c := Commit{Tree: root, Author: who, Committer: who, Message: "Tidy issue pagination"}
	o := db.Put(TypeCommit, AppendCommit(nil, &c))
	if got := o.ID.String(); got != "6ffffba7f216bf17a8de3a002e3d5348dcfcf5e3" {
		t.Fatalf("commit id = %s", got)
	}
	if o.Commit == nil || o.Commit.Tree != root || len(o.Commit.Parents) != 0 || o.Commit.Time != 1767225601 {
		t.Fatalf("commit info = %+v", o.Commit)
	}
	back, err := ParseCommit(o.Data)
	who.Zone = "+0000" // what an empty zone is written as
	if err != nil || back.Message != c.Message || back.Author != who || back.Committer != who || back.Tree != root {
		t.Fatalf("ParseCommit = %+v, %v", back, err)
	}

	// A pushed commit with extra headers and a multi-line signature still parses.
	raw := "tree " + root.String() + "\nparent " + o.ID.String() + "\nauthor A U Thor <a@example.com> 1700000000 +0200\n" +
		"committer C O Mitter <c@example.com> 1700000005 -0700\ngpgsig -----BEGIN-----\n line two\n -----END-----\nencoding UTF-8\n\nsubject\n\nbody\n"
	p := db.Put(TypeCommit, []byte(raw))
	if p.Commit == nil || len(p.Commit.Parents) != 1 || p.Commit.Parents[0] != o.ID || p.Commit.Time != 1700000005 {
		t.Fatalf("pushed commit info = %+v", p.Commit)
	}
	pc, err := ParseCommit(p.Data)
	if err != nil || pc.Message != "subject\n\nbody\n" || pc.Author.Name != "A U Thor" || pc.Author.Zone != "+0200" || Subject(pc.Message) != "subject" {
		t.Fatalf("pushed ParseCommit = %+v, %v", pc, err)
	}
}

func TestTagAndPeel(t *testing.T) {
	db := newDB()
	c := db.Put(TypeCommit, []byte("tree "+EmptyTree.String()+"\nauthor a <a@b> 1 +0000\ncommitter a <a@b> 1 +0000\n\nm\n"))
	tag := db.Put(TypeTag, []byte("object "+c.ID.String()+"\ntype commit\ntag v1\ntagger a <a@b> 1 +0000\n\nrelease\n"))
	target, typ, name, ok := ParseTag(tag.Data)
	if !ok || target != c.ID || typ != TypeCommit || name != "v1" {
		t.Fatalf("ParseTag = %s %v %q %v", target, typ, name, ok)
	}
	if db.Peel(tag.ID) != c || db.Peel(c.ID) != c {
		t.Fatal("Peel did not reach the commit")
	}
}

// history builds commits on a DB; each commit holds one file "f" with its own content unless files
// are given.
type history struct {
	t  *testing.T
	db *DB
	n  int64
}

func (h *history) commit(files map[string]string, parents ...OID) OID {
	h.n++
	root := Zero
	for path, content := range files {
		var err error
		if root, err = h.db.SetPath(root, path, ModeFile, h.db.Put(TypeBlob, []byte(content)).ID); err != nil {
			h.t.Fatal(err)
		}
	}
	if root.IsZero() {
		root = h.db.PutTree(nil)
	}
	who := Signature{Name: "t", Email: "t@example.com", When: 1000 + h.n}
	c := Commit{Tree: root, Parents: parents, Author: who, Committer: who, Message: "c\n"}
	return h.db.Put(TypeCommit, AppendCommit(nil, &c)).ID
}

func TestAncestryAndMergeBase(t *testing.T) {
	h := &history{t: t, db: newDB()}
	db := h.db
	//      a - b - c - d      (main)
	//           \
	//            e - f        (side)
	//  x                      (unrelated root)
	a := h.commit(map[string]string{"f": "a"})
	b := h.commit(map[string]string{"f": "b"}, a)
	c := h.commit(map[string]string{"f": "c"}, b)
	d := h.commit(map[string]string{"f": "d"}, c)
	e := h.commit(map[string]string{"f": "e"}, b)
	f := h.commit(map[string]string{"f": "f"}, e)
	x := h.commit(map[string]string{"f": "x"})

	yes := [][2]OID{{a, a}, {a, b}, {a, d}, {b, f}, {c, d}}
	no := [][2]OID{{d, a}, {c, f}, {f, d}, {x, d}, {a, x}}
	for _, p := range yes {
		if !db.IsAncestor(p[0], p[1]) {
			t.Errorf("IsAncestor(%s, %s) = false", p[0], p[1])
		}
	}
	for _, p := range no {
		if db.IsAncestor(p[0], p[1]) {
			t.Errorf("IsAncestor(%s, %s) = true", p[0], p[1])
		}
	}
	if db.IsAncestor(a, Zero) || db.IsAncestor(Zero, a) {
		t.Error("the zero id is nobody's ancestor or descendant")
	}
	for _, tc := range []struct{ p, q, want OID }{{d, f, b}, {f, d, b}, {d, c, c}, {a, f, a}, {d, d, d}} {
		if got, ok := db.MergeBase(tc.p, tc.q); !ok || got != tc.want {
			t.Errorf("MergeBase(%s, %s) = %s %v, want %s", tc.p, tc.q, got, ok, tc.want)
		}
	}
	if _, ok := db.MergeBase(d, x); ok {
		t.Error("unrelated histories have no merge base")
	}
	// A merge commit: its base with either parent's line is that parent.
	m := h.commit(map[string]string{"f": "m"}, d, f)
	if got, _ := db.MergeBase(m, f); got != f {
		t.Errorf("MergeBase(merge, side) = %s", got)
	}
	if n := db.CountAhead(f, d); n != 2 {
		t.Errorf("CountAhead(side, main) = %d, want 2", n)
	}
	if n := db.CountAhead(d, Zero); n != 4 {
		t.Errorf("CountAhead(main, none) = %d, want 4", n)
	}
	if n := db.CountAhead(b, d); n != 0 {
		t.Errorf("CountAhead(ancestor, tip) = %d, want 0", n)
	}
	var order []OID
	db.Walk(m, func(o *Object) bool { order = append(order, o.ID); return true })
	want := []OID{m, f, e, d, c, b, a} // newest committer time first
	if len(order) != len(want) {
		t.Fatalf("Walk visited %d commits, want %d", len(order), len(want))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("Walk order[%d] = %s, want %s", i, order[i], want[i])
		}
	}
}

func tree(t *testing.T, db *DB, files map[string]string) OID {
	t.Helper()
	root := Zero
	for path, content := range files {
		var err error
		if root, err = db.SetPath(root, path, ModeFile, db.Put(TypeBlob, []byte(content)).ID); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestMerge3(t *testing.T) {
	db := newDB()
	base := tree(t, db, map[string]string{"config/token.css": "base", "README": "r", "old": "o"})
	ours := tree(t, db, map[string]string{"config/token.css": "base", "README": "r2", "old": "o"})
	theirs := tree(t, db, map[string]string{"config/token.css": "base", "config/new.css": "n", "README": "r"})

	// Different paths changed on each side (and one deleted): a clean merge with all three effects.
	if _, ok := db.Merge3(base, ours, theirs, false); !ok {
		t.Fatal("clean merge reported as conflict")
	}
	before := db.Len()
	if _, ok := db.Merge3(base, ours, theirs, false); !ok || db.Len() != before {
		t.Fatal("a mergeability test must not store objects")
	}
	merged, ok := db.Merge3(base, ours, theirs, true)
	want := tree(t, db, map[string]string{"config/token.css": "base", "config/new.css": "n", "README": "r2"})
	if !ok || merged != want {
		t.Fatalf("merged tree = %s %v, want %s", merged, ok, want)
	}

	// Both sides changed the same file differently: conflict, in both modes.
	a := tree(t, db, map[string]string{"config/token.css": "base (a)", "README": "r", "old": "o"})
	b := tree(t, db, map[string]string{"config/token.css": "base (b)", "README": "r", "old": "o"})
	if _, ok := db.Merge3(base, a, b, false); ok {
		t.Fatal("conflict not detected")
	}
	if _, ok := db.Merge3(base, a, b, true); ok {
		t.Fatal("conflict not detected when writing")
	}
	// Both sides made the same change: fine.
	if got, ok := db.Merge3(base, a, a, true); !ok || got != a {
		t.Fatal("identical changes must merge")
	}
	// One side untouched: the other side wins as is.
	if got, ok := db.Merge3(base, base, b, true); !ok || got != b {
		t.Fatal("fast-forward style merge must return theirs")
	}
	// No common ancestor, disjoint files: union. Same path, different content: conflict.
	u1, u2 := tree(t, db, map[string]string{"one": "1"}), tree(t, db, map[string]string{"two": "2"})
	if got, ok := db.Merge3(Zero, u1, u2, true); !ok || got != tree(t, db, map[string]string{"one": "1", "two": "2"}) {
		t.Fatal("unrelated disjoint trees must merge to their union")
	}
	if _, ok := db.Merge3(Zero, u1, tree(t, db, map[string]string{"one": "other"}), false); ok {
		t.Fatal("unrelated trees with the same path must conflict")
	}
	// A file on one side where the other has a directory: conflict.
	if _, ok := db.Merge3(Zero, tree(t, db, map[string]string{"p": "file"}), tree(t, db, map[string]string{"p/q": "dir"}), false); ok {
		t.Fatal("file/directory clash must conflict")
	}
	// Everything deleted on one side, untouched on the other: the empty tree, stored.
	if got, ok := db.Merge3(base, db.PutTree(nil), base, true); !ok || got != EmptyTree {
		t.Fatalf("delete-all merge = %s %v", got, ok)
	}
}

func TestDiffTrees(t *testing.T) {
	db := newDB()
	a := tree(t, db, map[string]string{"keep": "k", "config/token.css": "one\ntwo\n", "gone/deep/file": "x\n", "swap": "file"})
	b := tree(t, db, map[string]string{"keep": "k", "config/token.css": "one\n2\nthree\n", "config/new.css": "a\nb\nc", "swap/inner": "dir"})
	got := db.DiffTrees(a, b)
	type row struct {
		path     string
		status   string
		add, del int
	}
	want := []row{
		{"config/new.css", "added", 3, 0},
		{"config/token.css", "modified", 2, 1},
		{"gone/deep/file", "removed", 0, 1},
		{"swap", "removed", 0, 1},
		{"swap/inner", "added", 1, 0},
	}
	if len(got) != len(want) {
		t.Fatalf("DiffTrees returned %d changes, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		c := &got[i]
		status := "modified"
		if c.Added() {
			status = "added"
		} else if c.Removed() {
			status = "removed"
		}
		add, del := db.ChangeStats(c)
		if c.Path != w.path || status != w.status || add != w.add || del != w.del {
			t.Errorf("change %d = %s %s +%d -%d, want %+v", i, c.Path, status, add, del, w)
		}
	}
	if len(db.DiffTrees(a, a)) != 0 || len(db.DiffTrees(Zero, Zero)) != 0 {
		t.Error("identical trees must not differ")
	}
	if all := db.DiffTrees(Zero, b); len(all) != 4 || !all[0].Added() {
		t.Errorf("diff against no tree = %+v", all)
	}
}

func TestLineStats(t *testing.T) {
	for _, tc := range []struct {
		old, new string
		add, del int
	}{
		{"", "", 0, 0},
		{"", "a\nb\n", 2, 0},
		{"", "a\nb", 2, 0},
		{"a\nb\n", "", 0, 2},
		{"a\nb\nc\n", "a\nb\nc\n", 0, 0},
		{"a\nb\nc\n", "a\nX\nc\n", 1, 1},
		{"a\nb\nc\n", "a\nc\n", 0, 1},
		{"a\nc\n", "a\nb\nc\n", 1, 0},
		{"a\nb", "a\nb\n", 1, 1}, // only the final newline changed: the last line differs
		{"a\nb\nc\nd\ne\n", "e\nd\nc\nb\na\n", 4, 4},
		{"bin\x00ary", "text\n", 0, 0},
	} {
		if add, del := LineStats([]byte(tc.old), []byte(tc.new)); add != tc.add || del != tc.del {
			t.Errorf("LineStats(%q, %q) = +%d -%d, want +%d -%d", tc.old, tc.new, add, del, tc.add, tc.del)
		}
	}
}

func TestDeflated(t *testing.T) {
	db := newDB()
	content := bytes.Repeat([]byte("forge "), 500)
	o := db.Put(TypeBlob, content)
	z := db.Deflated(o)
	if &z[0] != &db.Deflated(o)[0] {
		t.Fatal("the zlib stream must be built once")
	}
	r, err := zlib.NewReader(bytes.NewReader(z))
	if err != nil {
		t.Fatal(err)
	}
	back, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(back, content) {
		t.Fatalf("zlib stream does not inflate to the object: %v", err)
	}
	given := []byte("verbatim stream")
	if p := db.Add(OID{1}, TypeBlob, []byte("data"), given); !bytes.Equal(db.Deflated(p), given) {
		t.Fatal("a pushed object's own stream must be kept")
	}
}

func TestOIDParsing(t *testing.T) {
	const hex = "CE013625030ba8dba906f756967f9e9ca394464a"
	o, ok := ParseOID(hex)
	if !ok || o.String() != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("ParseOID = %s %v", o, ok)
	}
	if b, ok := ParseOIDBytes([]byte(hex)); !ok || b != o {
		t.Fatal("ParseOIDBytes disagrees with ParseOID")
	}
	for _, bad := range []string{"", "ce01", hex + "0", "ze013625030ba8dba906f756967f9e9ca394464a"} {
		if _, ok := ParseOID(bad); ok {
			t.Errorf("ParseOID(%q) accepted", bad)
		}
	}
	if !ValidPath("config/token.css") || ValidPath("") || ValidPath("/a") || ValidPath("a/") || ValidPath("a//b") ||
		ValidPath("a/../b") || ValidPath("./a") || ValidPath(".git/config") {
		t.Error("ValidPath gives a wrong verdict")
	}
}

// A fast-forward test whose answer is the new commit's parent must not allocate: it runs on every
// push.
func BenchmarkIsAncestorParent(b *testing.B) {
	h := &history{db: newDB()}
	tip := h.commit(map[string]string{"f": "0"})
	prev := tip
	for i := 0; i < 600; i++ {
		prev = tip
		tip = h.commit(map[string]string{"f": string(rune('a' + i%26))}, tip)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !h.db.IsAncestor(prev, tip) {
			b.Fatal("not an ancestor")
		}
	}
}

func BenchmarkPutBlob(b *testing.B) {
	db := newDB()
	data := bytes.Repeat([]byte("0123456789abcdef"), 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data[0], data[1], data[2], data[3] = byte(i), byte(i>>8), byte(i>>16), byte(i>>24)
		db.Put(TypeBlob, data)
	}
}
