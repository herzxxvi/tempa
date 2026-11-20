package gitobj

import (
	"bytes"
	"sort"
)

// Change is one path that differs between two trees. A Zero Old means the path was added, a Zero
// New means it was removed; both set means the content or mode changed.
type Change struct {
	Path             string
	Old, New         OID
	OldMode, NewMode uint32
}

// Added reports whether the path exists only in the new tree.
func (c *Change) Added() bool { return c.Old.IsZero() }

// Removed reports whether the path exists only in the old tree.
func (c *Change) Removed() bool { return c.New.IsZero() }

// DiffTrees lists the files that differ between tree a and tree b, sorted by path. Either side may
// be Zero (no tree). Directories never appear; a file replaced by a directory (or the reverse) shows
// as a removal plus additions.
func (db *DB) DiffTrees(a, b OID) []Change {
	var out []Change
	db.diff("", a, b, &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (db *DB) diff(prefix string, a, b OID, out *[]Change) {
	if a == b {
		return
	}
	ea, _ := db.Tree(a)
	eb, _ := db.Tree(b)
	for i := range ea {
		x := &ea[i]
		j := indexEntry(eb, x.Name)
		if j < 0 {
			db.diffSide(prefix, x, true, out)
			continue
		}
		y := &eb[j]
		switch {
		case x.IsDir() && y.IsDir():
			db.diff(prefix+x.Name+"/", x.ID, y.ID, out)
		case x.IsDir() != y.IsDir():
			db.diffSide(prefix, x, true, out)
			db.diffSide(prefix, y, false, out)
		case x.ID != y.ID || x.Mode != y.Mode:
			*out = append(*out, Change{Path: prefix + x.Name, Old: x.ID, New: y.ID, OldMode: x.Mode, NewMode: y.Mode})
		}
	}
	for j := range eb {
		if indexEntry(ea, eb[j].Name) < 0 {
			db.diffSide(prefix, &eb[j], false, out)
		}
	}
}

// diffSide records every file under e as removed (old side) or added (new side).
func (db *DB) diffSide(prefix string, e *Entry, removed bool, out *[]Change) {
	if e.IsDir() {
		sub, _ := db.Tree(e.ID)
		for i := range sub {
			db.diffSide(prefix+e.Name+"/", &sub[i], removed, out)
		}
		return
	}
	c := Change{Path: prefix + e.Name}
	if removed {
		c.Old, c.OldMode = e.ID, e.Mode
	} else {
		c.New, c.NewMode = e.ID, e.Mode
	}
	*out = append(*out, c)
}

// ChangeStats returns the added and deleted line counts of one change. Blobs that are missing,
// binary (a NUL in the first 8000 bytes) or gitlinks count as 0/0.
func (db *DB) ChangeStats(c *Change) (additions, deletions int) {
	var oldData, newData []byte
	if !c.Old.IsZero() {
		o := db.objs[c.Old]
		if o == nil || o.Type != TypeBlob {
			return 0, 0
		}
		oldData = o.Data
	}
	if !c.New.IsZero() {
		o := db.objs[c.New]
		if o == nil || o.Type != TypeBlob {
			return 0, 0
		}
		newData = o.Data
	}
	return LineStats(oldData, newData)
}

func isBinary(b []byte) bool {
	if len(b) > 8000 {
		b = b[:8000]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// splitLines cuts text into lines, each keeping its trailing newline, so "a" and "a\n" differ the
// way they do in a git diff. An empty text has no lines.
func splitLines(text []byte) [][]byte {
	if len(text) == 0 {
		return nil
	}
	lines := make([][]byte, 0, bytes.Count(text, []byte{'\n'})+1)
	for len(text) > 0 {
		i := bytes.IndexByte(text, '\n')
		if i < 0 {
			lines = append(lines, text)
			break
		}
		lines = append(lines, text[:i+1])
		text = text[i+1:]
	}
	return lines
}

// maxEditDistance bounds the diff search; past it the remaining lines are counted as fully replaced.
const maxEditDistance = 4096

// LineStats returns how many lines a line-based diff from old to new adds and deletes. It runs the
// greedy shortest-edit-script search of Myers' O(ND) algorithm and needs only the script's length D:
// additions - deletions is the difference in line counts and additions + deletions is D.
func LineStats(oldText, newText []byte) (additions, deletions int) {
	if isBinary(oldText) || isBinary(newText) {
		return 0, 0
	}
	a, b := splitLines(oldText), splitLines(newText)
	for len(a) > 0 && len(b) > 0 && bytes.Equal(a[0], b[0]) {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && bytes.Equal(a[len(a)-1], b[len(b)-1]) {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return m, n
	}
	limit := min(n+m, maxEditDistance)
	// v[k+limit] is the furthest x reached on diagonal k (x - y = k) with the current edit count.
	v := make([]int, 2*limit+2)
	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[k-1+limit] < v[k+1+limit]) {
				x = v[k+1+limit] // step down: take a line from b
			} else {
				x = v[k-1+limit] + 1 // step right: drop a line from a
			}
			y := x - k
			for x < n && y < m && bytes.Equal(a[x], b[y]) {
				x++
				y++
			}
			v[k+limit] = x
			if x >= n && y >= m {
				return (d + m - n) / 2, (d - m + n) / 2
			}
		}
	}
	return m, n
}
