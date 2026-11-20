package store

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// Kind is the type of object a global node id names.
type Kind uint8

const (
	KindNone Kind = iota
	KindUser
	KindRepo
	KindIssue
	KindPull
	KindComment
	KindLabel
)

// A node id is "<prefix>_<base64url, no padding, of '<TypeName>:<numeric id>'>", e.g. repository 17
// is "R_UmVwb3NpdG9yeToxNw". It is a pure function of the object's type and numeric id, so it never
// changes, and it is the same string as REST node_id and GraphQL id.
var kinds = [...]struct{ prefix, name string }{
	KindNone:    {"", ""},
	KindUser:    {"U", "User"},
	KindRepo:    {"R", "Repository"},
	KindIssue:   {"I", "Issue"},
	KindPull:    {"PR", "PullRequest"},
	KindComment: {"IC", "IssueComment"},
	KindLabel:   {"LA", "Label"},
}

// AppendNodeID appends the node id of (kind, id) to dst without allocating.
func AppendNodeID(dst []byte, kind Kind, id int64) []byte {
	var raw [40]byte
	payload := append(raw[:0], kinds[kind].name...)
	payload = append(payload, ':')
	payload = strconv.AppendInt(payload, id, 10)
	dst = append(dst, kinds[kind].prefix...)
	dst = append(dst, '_')
	return base64.RawURLEncoding.AppendEncode(dst, payload)
}

// NodeID returns the node id of (kind, id).
func NodeID(kind Kind, id int64) string {
	var buf [64]byte
	return string(AppendNodeID(buf[:0], kind, id))
}

// ViewerNodeID is the node id of the viewer.
var ViewerNodeID = NodeID(KindUser, ViewerID)

// ParseNodeID decodes a node id. ok is false for anything this forge did not issue.
func ParseNodeID(s string) (kind Kind, id int64, ok bool) {
	prefix, enc, found := strings.Cut(s, "_")
	if !found {
		return KindNone, 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return KindNone, 0, false
	}
	name, num, found := strings.Cut(string(payload), ":")
	if !found {
		return KindNone, 0, false
	}
	for k := KindUser; int(k) < len(kinds); k++ {
		if kinds[k].prefix == prefix && kinds[k].name == name {
			id, err := strconv.ParseInt(num, 10, 64)
			if err != nil || id < 1 || strconv.FormatInt(id, 10) != num {
				return KindNone, 0, false
			}
			return k, id, true
		}
	}
	return KindNone, 0, false
}

// Node resolves a node id to the live object: *Repo, *Issue, *Pull, *Comment or *Label. For the
// viewer's id it returns the Viewer value. It returns nil for an unknown or deleted object.
func (s *Store) Node(nodeID string) any {
	kind, id, ok := ParseNodeID(nodeID)
	if !ok {
		return nil
	}
	switch kind {
	case KindUser:
		if id == ViewerID {
			return Viewer{}
		}
	case KindRepo:
		if r := s.RepoByID(id); r != nil {
			return r
		}
	case KindIssue:
		if is := s.IssueByID(id); is != nil {
			return is
		}
	case KindPull:
		if p := s.PullByID(id); p != nil {
			return p
		}
	case KindComment:
		if c := s.CommentByID(id); c != nil {
			return c
		}
	case KindLabel:
		if l := s.LabelByID(id); l != nil {
			return l
		}
	}
	return nil
}

// Viewer is the type Node returns for the viewer's node id. The viewer's fields are the Viewer*
// constants.
type Viewer struct{}
