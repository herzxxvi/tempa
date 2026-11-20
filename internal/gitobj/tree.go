package gitobj

import (
	"errors"
	"sort"
	"strings"
)

// File modes git stores in tree entries.
const (
	ModeDir     uint32 = 0o40000
	ModeFile    uint32 = 0o100644
	ModeExec    uint32 = 0o100755
	ModeSymlink uint32 = 0o120000
	ModeGitlink uint32 = 0o160000
)

// Entry is one line of a tree: a mode, a name without slashes, and the id of a blob or subtree.
type Entry struct {
	Mode uint32
	Name string
	ID   OID
}

// IsDir reports whether the entry names a subtree.
func (e Entry) IsDir() bool { return e.Mode == ModeDir }

var (
	// ErrNoTree: an id that should name a tree does not (missing object or another type).
	ErrNoTree = errors.New("gitobj: not a tree")
	// ErrNotDir: a path walks through an entry that is not a directory.
	ErrNotDir = errors.New("gitobj: path component is not a directory")
	// ErrNoEntry: the path does not exist in the tree.
	ErrNoEntry = errors.New("gitobj: no such path")
)

// nextEntry decodes the entry at the front of tree data and returns the bytes after it.
func nextEntry(data []byte) (mode uint32, name []byte, id OID, rest []byte, ok bool) {
	i := 0
	for ; i < len(data) && data[i] != ' '; i++ {
		c := data[i]
		if c < '0' || c > '7' {
			return 0, nil, Zero, nil, false
		}
		mode = mode<<3 | uint32(c-'0')
	}
	if i == 0 || i >= len(data) {
		return 0, nil, Zero, nil, false
	}
	i++
	start := i
	for ; i < len(data) && data[i] != 0; i++ {
	}
	if i == start || len(data)-i < 21 {
		return 0, nil, Zero, nil, false
	}
	name = data[start:i]
	copy(id[:], data[i+1:i+21])
	return mode, name, id, data[i+21:], true
}

// ParseTree decodes a tree object into its entries, in stored order.
func ParseTree(data []byte) ([]Entry, error) {
	var out []Entry
	for len(data) > 0 {
		mode, name, id, rest, ok := nextEntry(data)
		if !ok {
			return nil, ErrMalformed
		}
		out = append(out, Entry{Mode: mode, Name: string(name), ID: id})
		data = rest
	}
	return out, nil
}

// entryLess is git's tree order: names compare as bytes, and a directory compares as if its name
// ended with '/'.
func entryLess(a, b *Entry) bool {
	n := min(len(a.Name), len(b.Name))
	if c := strings.Compare(a.Name[:n], b.Name[:n]); c != 0 {
		return c < 0
	}
	var ac, bc byte
	if len(a.Name) > n {
		ac = a.Name[n]
	} else if a.IsDir() {
		ac = '/'
	}
	if len(b.Name) > n {
		bc = b.Name[n]
	} else if b.IsDir() {
		bc = '/'
	}
	return ac < bc
}

// SortEntries puts entries into git's tree order.
func SortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool { return entryLess(&entries[i], &entries[j]) })
}

// AppendTree appends the encoding of a tree. entries must already be in tree order.
func AppendTree(dst []byte, entries []Entry) []byte {
	for i := range entries {
		e := &entries[i]
		dst = appendOctal(dst, e.Mode)
		dst = append(dst, ' ')
		dst = append(dst, e.Name...)
		dst = append(dst, 0)
		dst = append(dst, e.ID[:]...)
	}
	return dst
}

// appendOctal writes a mode the way git does: octal, no leading zero ("40000", "100644").
func appendOctal(dst []byte, v uint32) []byte {
	var b [12]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + v&7)
		v >>= 3
		if v == 0 {
			break
		}
	}
	return append(dst, b[i:]...)
}

// Tree returns the decoded entries of a tree. Zero and EmptyTree yield no entries even when the
// empty tree object is not stored. The returned slice is the caller's to modify.
func (db *DB) Tree(id OID) ([]Entry, error) {
	if id.IsZero() || id == EmptyTree {
		return nil, nil
	}
	o := db.objs[id]
	if o == nil || o.Type != TypeTree {
		return nil, ErrNoTree
	}
	return ParseTree(o.Data)
}

// PutTree sorts entries, stores the tree and returns its id. No entries stores the empty tree.
func (db *DB) PutTree(entries []Entry) OID {
	SortEntries(entries)
	return db.Put(TypeTree, AppendTree(nil, entries)).ID
}

// Find resolves a slash-separated path below root. The empty path names root itself. It does not
// allocate.
func (db *DB) Find(root OID, path string) (Entry, bool) {
	cur := Entry{Mode: ModeDir, ID: root}
	for path != "" {
		seg := path
		if i := strings.IndexByte(path, '/'); i >= 0 {
			seg, path = path[:i], path[i+1:]
		} else {
			path = ""
		}
		if !cur.IsDir() {
			return Entry{}, false
		}
		o := db.objs[cur.ID]
		if o == nil || o.Type != TypeTree {
			return Entry{}, false
		}
		found := false
		for data := o.Data; len(data) > 0; {
			mode, name, id, rest, ok := nextEntry(data)
			if !ok {
				return Entry{}, false
			}
			if string(name) == seg {
				cur = Entry{Mode: mode, Name: seg, ID: id}
				found = true
				break
			}
			data = rest
		}
		if !found {
			return Entry{}, false
		}
	}
	return cur, true
}

// SetPath returns the root tree that results from putting (mode, id) at path below root, creating
// missing directories and replacing whatever was at path. root may be Zero for "no tree yet".
// ErrNotDir means a leading component of path exists and is not a directory.
func (db *DB) SetPath(root OID, path string, mode uint32, id OID) (OID, error) {
	seg, rest, nested := strings.Cut(path, "/")
	entries, err := db.Tree(root)
	if err != nil {
		return Zero, err
	}
	at := indexEntry(entries, seg)
	e := Entry{Mode: mode, Name: seg, ID: id}
	if nested {
		child := Zero
		if at >= 0 {
			if !entries[at].IsDir() {
				return Zero, ErrNotDir
			}
			child = entries[at].ID
		}
		sub, err := db.SetPath(child, rest, mode, id)
		if err != nil {
			return Zero, err
		}
		e = Entry{Mode: ModeDir, Name: seg, ID: sub}
	}
	if at >= 0 {
		entries[at] = e
	} else {
		entries = append(entries, e)
	}
	return db.PutTree(entries), nil
}

// RemovePath returns the root tree without the entry at path. Directories left empty are removed
// too, so the result can be the empty tree. ErrNoEntry means path does not exist.
func (db *DB) RemovePath(root OID, path string) (OID, error) {
	id, err := db.removePath(root, path)
	if err != nil {
		return Zero, err
	}
	if id.IsZero() {
		return db.PutTree(nil), nil
	}
	return id, nil
}

// removePath is RemovePath that reports an emptied tree as Zero instead of storing it.
func (db *DB) removePath(tree OID, path string) (OID, error) {
	seg, rest, nested := strings.Cut(path, "/")
	entries, err := db.Tree(tree)
	if err != nil {
		return Zero, err
	}
	at := indexEntry(entries, seg)
	if at < 0 {
		return Zero, ErrNoEntry
	}
	drop := true
	if nested {
		if !entries[at].IsDir() {
			return Zero, ErrNoEntry
		}
		sub, err := db.removePath(entries[at].ID, rest)
		if err != nil {
			return Zero, err
		}
		if !sub.IsZero() {
			entries[at].ID = sub
			drop = false
		}
	}
	if drop {
		entries = append(entries[:at], entries[at+1:]...)
	}
	if len(entries) == 0 {
		return Zero, nil
	}
	return db.PutTree(entries), nil
}

func indexEntry(entries []Entry, name string) int {
	for i := range entries {
		if entries[i].Name == name {
			return i
		}
	}
	return -1
}

// ValidPath reports whether path is usable as a file path in a tree: not empty, no leading or
// trailing slash, and no empty, "." , ".." or ".git" component, no NUL.
func ValidPath(path string) bool {
	if path == "" {
		return false
	}
	for path != "" {
		seg, rest, more := strings.Cut(path, "/")
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") || strings.IndexByte(seg, 0) >= 0 {
			return false
		}
		if more && rest == "" {
			return false
		}
		path = rest
	}
	return true
}

// NextEntry decodes the tree entry at the front of data and returns the bytes after it, without
// allocating: name is a view into data. ok is false when data does not start with a well-formed
// entry. Loop until rest is empty to visit a whole tree.
func NextEntry(data []byte) (mode uint32, name []byte, id OID, rest []byte, ok bool) {
	return nextEntry(data)
}
