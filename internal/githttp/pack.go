package githttp

import (
	"bytes"
	"compress/flate"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"hash/adler32"
	"io"
	"sort"

	"forge/internal/gitobj"
)

// Pack format, version 2: "PACK", version, object count, then one entry per object (a type-and-size
// header followed by a zlib stream), then the SHA-1 of everything before it. An entry is either a
// whole object or a delta against a base named by offset (OFS_DELTA) or by id (REF_DELTA).

const (
	packOfsDelta = 6
	packRefDelta = 7
)

var (
	errPackHeader   = errors.New("not a pack")
	errPackVersion  = errors.New("unsupported pack version")
	errPackChecksum = errors.New("pack checksum mismatch")
	errPackShort    = errors.New("pack is truncated")
	errPackTrailing = errors.New("pack has bytes after its last object")
	errPackType     = errors.New("pack has an entry of unknown type")
	errPackZlib     = errors.New("pack entry is not a zlib stream")
	errPackSize     = errors.New("pack entry does not inflate to its declared size")
	errPackAdler    = errors.New("pack entry fails its zlib checksum")
	errPackDelta    = errors.New("pack has a malformed delta")
	errPackBase     = errors.New("pack has a delta whose base is missing")
)

// unpacker reads pushed packs into an object database. One instance serves every push (the store
// mutex serialises them); after its buffers have grown it reads a pack without allocating.
type unpacker struct {
	br  bytes.Reader
	fr  io.ReadCloser // raw deflate reader, reused through flate.Resetter
	buf []byte        // inflated content of the entry being read
	one [1]byte

	added   []*gitobj.Object // objects this pack added to the database
	entries []packEntry      // every entry's offset and, once known, its object (OFS_DELTA bases)
	pending []pendingDelta
}

type packEntry struct {
	offset int
	obj    *gitobj.Object
}

type pendingDelta struct {
	entry      int // index into entries
	byID       bool
	baseOffset int
	baseID     gitobj.OID
	delta      []byte
}

// inflate decodes the zlib stream at the front of z, which must inflate to exactly size bytes. It
// returns the content (valid until the next call) and the length of the stream, so the caller can
// keep z[:n] verbatim and find the next entry at z[n:].
//
// The zlib container is handled here (2 header bytes, raw deflate, Adler-32) rather than through
// compress/zlib: the deflate reader takes bytes one at a time from a *bytes.Reader, so when it
// reports EOF the reader sits exactly at the end of the deflate data, and a reused deflate reader
// allocates nothing.
func (u *unpacker) inflate(z []byte, size int) (content []byte, n int, err error) {
	if len(z) < 6 || z[0]&0x0f != 8 || (uint(z[0])<<8|uint(z[1]))%31 != 0 || z[1]&0x20 != 0 {
		return nil, 0, errPackZlib
	}
	// Deflate cannot expand beyond roughly 1032:1, so a larger claim is a lie; refuse it before
	// allocating for it.
	if size < 0 || size > len(z)*1100+64 {
		return nil, 0, errPackSize
	}
	if cap(u.buf) < size {
		u.buf = make([]byte, size+size/4+1024)
	}
	content = u.buf[:size]
	u.br.Reset(z[2:])
	if u.fr == nil {
		u.fr = flate.NewReader(&u.br)
	} else if err := u.fr.(flate.Resetter).Reset(&u.br, nil); err != nil {
		return nil, 0, errPackZlib
	}
	if _, err := io.ReadFull(u.fr, content); err != nil {
		return nil, 0, errPackSize
	}
	// One more read must find the end of the stream: more data means the declared size was short.
	if got, err := u.fr.Read(u.one[:]); got != 0 || err != io.EOF {
		return nil, 0, errPackSize
	}
	end := len(z) - u.br.Len()
	if len(z)-end < 4 {
		return nil, 0, errPackShort
	}
	if adler32.Checksum(content) != binary.BigEndian.Uint32(z[end:]) {
		return nil, 0, errPackAdler
	}
	return content, end + 4, nil
}

// unpack verifies a pack and stores its objects in db. Objects the database already has are left
// alone. u.added lists what was new. On an error nothing useful has been stored and the caller
// must call rollback.
func (u *unpacker) unpack(db *gitobj.DB, pack []byte) error {
	u.added, u.entries, u.pending = u.added[:0], u.entries[:0], u.pending[:0]
	if len(pack) < 12+sha1.Size || string(pack[:4]) != "PACK" {
		return errPackHeader
	}
	if v := binary.BigEndian.Uint32(pack[4:]); v != 2 && v != 3 {
		return errPackVersion
	}
	end := len(pack) - sha1.Size
	if sum := sha1.Sum(pack[:end]); string(sum[:]) != string(pack[end:]) {
		return errPackChecksum
	}
	count := int(binary.BigEndian.Uint32(pack[8:]))
	pos := 12
	for i := 0; i < count; i++ {
		start := pos
		if pos >= end {
			return errPackShort
		}
		b := pack[pos]
		pos++
		typ, size, shift := gitobj.Type(b>>4&7), int(b&15), 4
		for b&0x80 != 0 {
			if pos >= end || shift > 56 {
				return errPackShort
			}
			b = pack[pos]
			pos++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		switch typ {
		case gitobj.TypeCommit, gitobj.TypeTree, gitobj.TypeBlob, gitobj.TypeTag:
			content, n, err := u.inflate(pack[pos:end], size)
			if err != nil {
				return err
			}
			id := db.ID(typ, content)
			obj := db.Get(id)
			if obj == nil {
				obj = db.Add(id, typ, content, pack[pos:pos+n])
				u.added = append(u.added, obj)
			}
			u.entries = append(u.entries, packEntry{start, obj})
			pos += n
		case packOfsDelta, packRefDelta:
			d := pendingDelta{entry: len(u.entries)}
			if typ == packRefDelta {
				if end-pos < 20 {
					return errPackShort
				}
				d.byID = true
				copy(d.baseID[:], pack[pos:])
				pos += 20
			} else {
				// The distance back to the base: big-endian base-128 with an offset of one per
				// continuation byte.
				if pos >= end {
					return errPackShort
				}
				b = pack[pos]
				pos++
				back := int(b & 0x7f)
				for b&0x80 != 0 {
					if pos >= end || back > 1<<40 {
						return errPackShort
					}
					b = pack[pos]
					pos++
					back = (back+1)<<7 | int(b&0x7f)
				}
				if back <= 0 || back > start {
					return errPackDelta
				}
				d.baseOffset = start - back
			}
			content, n, err := u.inflate(pack[pos:end], size)
			if err != nil {
				return err
			}
			d.delta = bytes.Clone(content)
			u.pending = append(u.pending, d)
			u.entries = append(u.entries, packEntry{start, nil})
			pos += n
		default:
			return errPackType
		}
	}
	if pos != end {
		return errPackTrailing
	}
	return u.resolve(db)
}

// resolve turns the pack's deltas into objects. A base can be another entry of the pack (possibly a
// delta itself, resolved in an earlier round) or, for a thin pack, an object already stored.
func (u *unpacker) resolve(db *gitobj.DB) error {
	for left := len(u.pending); left > 0; {
		progress := false
		for i := range u.pending {
			d := &u.pending[i]
			if d.delta == nil {
				continue
			}
			var base *gitobj.Object
			if d.byID {
				base = db.Get(d.baseID)
			} else {
				// Entries are in pack order, so their offsets are sorted.
				j := sort.Search(len(u.entries), func(k int) bool { return u.entries[k].offset >= d.baseOffset })
				if j == len(u.entries) || u.entries[j].offset != d.baseOffset {
					return errPackDelta
				}
				base = u.entries[j].obj
			}
			if base == nil {
				continue
			}
			content, err := applyDelta(base.Data, d.delta)
			if err != nil {
				return err
			}
			id := db.ID(base.Type, content)
			obj := db.Get(id)
			if obj == nil {
				obj = db.Add(id, base.Type, content, nil)
				u.added = append(u.added, obj)
			}
			u.entries[d.entry].obj = obj
			d.delta = nil
			left--
			progress = true
		}
		if !progress {
			return errPackBase
		}
	}
	return nil
}

// rollback takes the objects of the last unpack out of the database again.
func (u *unpacker) rollback(db *gitobj.DB) {
	for _, o := range u.added {
		db.Remove(o.ID)
	}
	u.added = u.added[:0]
}

// deltaSize reads one of the two size varints at the start of a delta.
func deltaSize(delta []byte) (size int, rest []byte, ok bool) {
	for shift := 0; len(delta) > 0 && shift < 56; shift += 7 {
		b := delta[0]
		delta = delta[1:]
		size |= int(b&0x7f) << shift
		if b&0x80 == 0 {
			return size, delta, true
		}
	}
	return 0, nil, false
}

// applyDelta rebuilds an object from its base and a git delta: a list of "copy this range of the
// base" and "insert these literal bytes" instructions.
func applyDelta(base, delta []byte) ([]byte, error) {
	baseSize, delta, ok := deltaSize(delta)
	if !ok || baseSize != len(base) {
		return nil, errPackDelta
	}
	size, delta, ok := deltaSize(delta)
	if !ok || size > 1<<31 {
		return nil, errPackDelta
	}
	out := make([]byte, 0, size)
	for len(delta) > 0 {
		op := delta[0]
		delta = delta[1:]
		switch {
		case op&0x80 != 0: // copy: the low bits say which offset and length bytes follow
			var offset, n int
			for i := uint(0); i < 4; i++ {
				if op&(1<<i) != 0 {
					if len(delta) == 0 {
						return nil, errPackDelta
					}
					offset |= int(delta[0]) << (8 * i)
					delta = delta[1:]
				}
			}
			for i := uint(0); i < 3; i++ {
				if op&(0x10<<i) != 0 {
					if len(delta) == 0 {
						return nil, errPackDelta
					}
					n |= int(delta[0]) << (8 * i)
					delta = delta[1:]
				}
			}
			if n == 0 {
				n = 0x10000
			}
			if offset+n > len(base) || len(out)+n > size {
				return nil, errPackDelta
			}
			out = append(out, base[offset:offset+n]...)
		case op != 0: // insert: op literal bytes follow
			n := int(op)
			if n > len(delta) || len(out)+n > size {
				return nil, errPackDelta
			}
			out = append(out, delta[:n]...)
			delta = delta[n:]
		default:
			return nil, errPackDelta
		}
	}
	if len(out) != size {
		return nil, errPackDelta
	}
	return out, nil
}

// appendPack appends a version 2 pack holding exactly the given objects, each as a whole
// (undeltified) entry, followed by the pack's checksum. An object's zlib stream is the one it
// arrived with when it was pushed, so nothing is compressed again.
func appendPack(dst []byte, db *gitobj.DB, objects []*gitobj.Object) []byte {
	start := len(dst)
	dst = append(dst, 'P', 'A', 'C', 'K', 0, 0, 0, 2)
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(objects)))
	for _, o := range objects {
		size := len(o.Data)
		b := byte(o.Type)<<4 | byte(size&15)
		for size >>= 4; size > 0; size >>= 7 {
			dst = append(dst, b|0x80)
			b = byte(size & 0x7f)
		}
		dst = append(dst, b)
		dst = append(dst, db.Deflated(o)...)
	}
	sum := sha1.Sum(dst[start:])
	return append(dst, sum[:]...)
}
