package gitobj

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"hash"
	"strconv"
)

// Arena hands out append-only byte storage in large chunks, so storing an object costs no allocation
// of its own and object bytes sit next to each other. Bytes are never reclaimed individually.
type Arena struct {
	cur []byte
}

const (
	arenaChunk = 1 << 20
	arenaLarge = 64 << 10 // anything bigger gets its own allocation
)

// Copy returns a private copy of b that stays valid for the life of the process.
func (a *Arena) Copy(b []byte) []byte {
	n := len(b)
	if n == 0 {
		return nil
	}
	if n > arenaLarge {
		return bytes.Clone(b)
	}
	if cap(a.cur)-len(a.cur) < n {
		a.cur = make([]byte, 0, arenaChunk)
	}
	off := len(a.cur)
	a.cur = append(a.cur, b...)
	return a.cur[off : off+n : off+n]
}

// Object is one stored git object. Data is the exact content the id was computed from and is never
// modified or re-encoded. Z, when set, is a zlib stream that inflates to Data (kept verbatim from a
// pushed pack, or produced once by DB.Deflated).
type Object struct {
	ID     OID
	Type   Type
	Data   []byte
	Z      []byte
	Commit *CommitInfo // parsed header of a commit; nil for other types and for unparsable commits
}

// CommitInfo is the part of a commit the graph algorithms need, parsed once when the object is stored.
type CommitInfo struct {
	Tree    OID
	Parents []OID
	Time    int64 // committer timestamp, seconds since the Unix epoch

	gen  uint32 // 1 + the largest generation among the parents; 0 = not computed yet
	seen uint32 // epoch stamp of the last walk that visited this commit
}

// DB is the object database of one repository.
type DB struct {
	objs  map[OID]*Object
	arena *Arena

	hasher hash.Hash
	sum    [sha1.Size]byte
	hdr    [32]byte

	oslab []Object
	cslab []CommitInfo
	pslab []OID

	epoch uint32
	stack []*Object

	zw   *zlib.Writer
	zbuf bytes.Buffer
}

// NewDB returns an empty database that stores object bytes in arena.
func NewDB(arena *Arena) *DB {
	return &DB{arena: arena, hasher: sha1.New()}
}

// Len returns the number of stored objects.
func (db *DB) Len() int { return len(db.objs) }

// Get returns the object with the given id, or nil.
func (db *DB) Get(id OID) *Object { return db.objs[id] }

// Has reports whether the object is stored.
func (db *DB) Has(id OID) bool { return db.objs[id] != nil }

// GetCommit returns the object when it is a parsable commit, else nil.
func (db *DB) GetCommit(id OID) *Object {
	if o := db.objs[id]; o != nil && o.Commit != nil {
		return o
	}
	return nil
}

// ID computes the object id of (t, data): SHA-1 over "<type> <len>\0" followed by data.
func (db *DB) ID(t Type, data []byte) OID {
	h := append(db.hdr[:0], t.String()...)
	h = append(h, ' ')
	h = strconv.AppendInt(h, int64(len(data)), 10)
	h = append(h, 0)
	db.hasher.Reset()
	db.hasher.Write(h)
	db.hasher.Write(data)
	return OID(db.hasher.Sum(db.sum[:0]))
}

// Put stores an object built by the server and returns it. data is copied; storing the same content
// twice returns the existing object.
func (db *DB) Put(t Type, data []byte) *Object {
	id := db.ID(t, data)
	if o := db.objs[id]; o != nil {
		return o
	}
	return db.insert(id, t, db.arena.Copy(data), nil)
}

// Add stores an object whose id the caller has already computed and verified (pack ingestion). data
// and z are copied; z may be nil. An object that is already stored is returned unchanged.
func (db *DB) Add(id OID, t Type, data, z []byte) *Object {
	if o := db.objs[id]; o != nil {
		return o
	}
	return db.insert(id, t, db.arena.Copy(data), db.arena.Copy(z))
}

func (db *DB) insert(id OID, t Type, data, z []byte) *Object {
	if db.objs == nil {
		db.objs = make(map[OID]*Object, 1024)
	}
	if len(db.oslab) == cap(db.oslab) {
		db.oslab = make([]Object, 0, 256)
	}
	db.oslab = append(db.oslab, Object{ID: id, Type: t, Data: data, Z: z})
	o := &db.oslab[len(db.oslab)-1]
	if t == TypeCommit {
		o.Commit = db.parseInfo(data)
	}
	db.objs[id] = o
	return o
}

// parseInfo reads the tree, parents and committer time out of a commit. It returns nil when the
// header is not a well-formed commit header.
func (db *DB) parseInfo(data []byte) *CommitInfo {
	const treeLine = len("tree ") + 40 + 1
	if len(data) < treeLine || string(data[:5]) != "tree " || data[treeLine-1] != '\n' {
		return nil
	}
	tree, ok := ParseOIDBytes(data[5:45])
	if !ok {
		return nil
	}
	rest := data[treeLine:]
	const parentLine = len("parent ") + 40 + 1
	first := len(db.pslab)
	for len(rest) >= parentLine && string(rest[:7]) == "parent " && rest[parentLine-1] == '\n' {
		p, ok := ParseOIDBytes(rest[7:47])
		if !ok {
			db.pslab = db.pslab[:first]
			return nil
		}
		if len(db.pslab) == cap(db.pslab) {
			// Start a new slab, carrying over this commit's parents so they stay contiguous.
			grown := make([]OID, 0, 256)
			grown = append(grown, db.pslab[first:]...)
			db.pslab, first = grown, 0
		}
		db.pslab = append(db.pslab, p)
		rest = rest[parentLine:]
	}
	if len(db.cslab) == cap(db.cslab) {
		db.cslab = make([]CommitInfo, 0, 256)
	}
	db.cslab = append(db.cslab, CommitInfo{Tree: tree, Time: committerTime(rest)})
	ci := &db.cslab[len(db.cslab)-1]
	if n := len(db.pslab) - first; n > 0 {
		ci.Parents = db.pslab[first:len(db.pslab):len(db.pslab)]
	}
	return ci
}

// committerTime finds the "committer" header line in the rest of a commit header and returns its
// timestamp, or 0.
func committerTime(hdr []byte) int64 {
	for len(hdr) > 0 && hdr[0] != '\n' {
		end := bytes.IndexByte(hdr, '\n')
		if end < 0 {
			end = len(hdr)
		}
		line := hdr[:end]
		if len(line) > 10 && string(line[:10]) == "committer " {
			// "... <email> <seconds> <zone>"
			sp := bytes.LastIndexByte(line, ' ')
			if sp < 0 {
				return 0
			}
			line = line[:sp]
			sp = bytes.LastIndexByte(line, ' ')
			var t int64
			for _, c := range line[sp+1:] {
				if c < '0' || c > '9' {
					return 0
				}
				t = t*10 + int64(c-'0')
			}
			return t
		}
		if end == len(hdr) {
			break
		}
		hdr = hdr[end+1:]
	}
	return 0
}

// Deflated returns a zlib stream that inflates to o.Data, building and caching it on first use.
// Server-made objects are stored uncompressed inside the stream: it is valid zlib, costs a copy
// instead of a compression pass, and objects that arrived in a pack keep the client's own stream.
func (db *DB) Deflated(o *Object) []byte {
	if o.Z != nil {
		return o.Z
	}
	db.zbuf.Reset()
	if db.zw == nil {
		db.zw, _ = zlib.NewWriterLevel(&db.zbuf, zlib.NoCompression)
	} else {
		db.zw.Reset(&db.zbuf)
	}
	db.zw.Write(o.Data)
	db.zw.Close()
	o.Z = db.arena.Copy(db.zbuf.Bytes())
	return o.Z
}

// Peel follows annotated tags from id to the first object that is not a tag. It returns nil when
// the chain leaves the database.
func (db *DB) Peel(id OID) *Object {
	for depth := 0; depth < 32; depth++ {
		o := db.objs[id]
		if o == nil || o.Type != TypeTag {
			return o
		}
		target, _, _, ok := ParseTag(o.Data)
		if !ok {
			return nil
		}
		id = target
	}
	return nil
}
