// Package gitobj is the git object model of the forge: object ids, the four object types and their
// canonical byte encodings, an in-memory object database, and the tree and history algorithms every
// surface shares (path edits, diffs, three-way merges, ancestry, merge bases).
//
// Nothing in this package locks. A DB belongs to one repository and is only touched while the store
// mutex is held (see docs/ARCHITECTURE.md).
package gitobj

// OID is a SHA-1 object id in binary form. The zero value means "no object".
type OID [20]byte

// Zero is the all-zero id git uses for "no object" (ref creation and deletion commands).
var Zero OID

// EmptyTree is the id of the tree with no entries.
var EmptyTree = MustParseOID("4b825dc642cb6eb9a060e54bf8d69288fbee4904")

const hexDigits = "0123456789abcdef"

// hexValue maps an ASCII byte to its hex value, or 0xff when it is not a hex digit.
var hexValue = func() (t [256]byte) {
	for i := range t {
		t[i] = 0xff
	}
	for i := byte(0); i < 10; i++ {
		t['0'+i] = i
	}
	for i := byte(0); i < 6; i++ {
		t['a'+i] = 10 + i
		t['A'+i] = 10 + i
	}
	return t
}()

// IsZero reports whether o is the zero id.
func (o OID) IsZero() bool { return o == Zero }

// String returns the 40 lowercase hex digits of o.
func (o OID) String() string {
	var b [40]byte
	o.putHex(b[:])
	return string(b[:])
}

// AppendHex appends the 40 lowercase hex digits of o to dst.
func (o OID) AppendHex(dst []byte) []byte {
	n := len(dst)
	dst = append(dst, "0000000000000000000000000000000000000000"...)
	o.putHex(dst[n:])
	return dst
}

func (o OID) putHex(b []byte) {
	for i, v := range o {
		b[2*i] = hexDigits[v>>4]
		b[2*i+1] = hexDigits[v&15]
	}
}

// ParseOID decodes 40 hex digits (either case). ok is false for anything else.
func ParseOID(s string) (o OID, ok bool) {
	if len(s) != 40 {
		return Zero, false
	}
	for i := 0; i < 20; i++ {
		hi, lo := hexValue[s[2*i]], hexValue[s[2*i+1]]
		if hi|lo == 0xff {
			return Zero, false
		}
		o[i] = hi<<4 | lo
	}
	return o, true
}

// ParseOIDBytes is ParseOID for a byte slice; it does not allocate.
func ParseOIDBytes(b []byte) (o OID, ok bool) {
	if len(b) != 40 {
		return Zero, false
	}
	for i := 0; i < 20; i++ {
		hi, lo := hexValue[b[2*i]], hexValue[b[2*i+1]]
		if hi|lo == 0xff {
			return Zero, false
		}
		o[i] = hi<<4 | lo
	}
	return o, true
}

// MustParseOID is ParseOID for constants; it panics on a malformed id.
func MustParseOID(s string) OID {
	o, ok := ParseOID(s)
	if !ok {
		panic("gitobj: malformed object id " + s)
	}
	return o
}

// Type is a git object type. The values are the type codes of the pack format.
type Type uint8

const (
	TypeNone   Type = 0
	TypeCommit Type = 1
	TypeTree   Type = 2
	TypeBlob   Type = 3
	TypeTag    Type = 4
)

// String returns the type name git hashes with ("commit", "tree", "blob", "tag").
func (t Type) String() string {
	switch t {
	case TypeCommit:
		return "commit"
	case TypeTree:
		return "tree"
	case TypeBlob:
		return "blob"
	case TypeTag:
		return "tag"
	}
	return ""
}

// ParseType is the inverse of Type.String.
func ParseType(s []byte) Type {
	switch string(s) {
	case "commit":
		return TypeCommit
	case "tree":
		return TypeTree
	case "blob":
		return TypeBlob
	case "tag":
		return TypeTag
	}
	return TypeNone
}
