package store

import "forge/internal/gitobj"

// Ref access for the git surface, which holds ref names as bytes inside a request body and must not
// allocate to look one up or to move it.

func (r *Repo) refIndexB(name []byte) (int, bool) {
	lo, hi := 0, len(r.refs)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if r.refs[mid].Name < string(name) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < len(r.refs) && r.refs[lo].Name == string(name)
}

// RefB is Ref for a name held as bytes.
func (r *Repo) RefB(name []byte) (gitobj.OID, bool) {
	if i, ok := r.refIndexB(name); ok {
		return r.refs[i].ID, true
	}
	return gitobj.Zero, false
}

// SetRefB is SetRef for a name held as bytes; moving an existing ref does not allocate.
func (r *Repo) SetRefB(name []byte, id gitobj.OID) {
	if i, ok := r.refIndexB(name); ok {
		if r.refs[i].ID != id {
			r.refs[i].ID = id
			r.refsChanged()
		}
		return
	}
	r.SetRef(string(name), id)
}

// RemoveRefB is RemoveRef for a name held as bytes.
func (r *Repo) RemoveRefB(name []byte) bool {
	i, ok := r.refIndexB(name)
	if !ok {
		return false
	}
	r.refs = append(r.refs[:i], r.refs[i+1:]...)
	r.refsChanged()
	return true
}
