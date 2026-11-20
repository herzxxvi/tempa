package gitobj

import (
	"bytes"
	"errors"
	"strconv"
)

// Signature is the author or committer of a commit.
type Signature struct {
	Name  string
	Email string
	When  int64  // seconds since the Unix epoch
	Zone  string // "+0000" style; empty means "+0000"
}

// Commit is a decoded commit object.
type Commit struct {
	Tree      OID
	Parents   []OID
	Author    Signature
	Committer Signature
	Message   string // everything after the blank line, byte for byte
}

// ErrMalformed is returned when object bytes do not parse as the expected type.
var ErrMalformed = errors.New("gitobj: malformed object")

func appendSignature(dst []byte, kind string, s *Signature) []byte {
	dst = append(dst, kind...)
	dst = append(dst, ' ')
	dst = appendClean(dst, s.Name)
	dst = append(dst, " <"...)
	dst = appendClean(dst, s.Email)
	dst = append(dst, "> "...)
	dst = strconv.AppendInt(dst, s.When, 10)
	dst = append(dst, ' ')
	if s.Zone == "" {
		dst = append(dst, "+0000"...)
	} else {
		dst = append(dst, s.Zone...)
	}
	return append(dst, '\n')
}

// appendClean appends s without the bytes that would break a header line ('<', '>', newline, NUL).
func appendClean(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<', '>', '\n', '\r', 0:
		default:
			dst = append(dst, s[i])
		}
	}
	return dst
}

// AppendCommit appends the canonical encoding of c: tree, parents, author, committer, a blank line
// and the message exactly as given.
func AppendCommit(dst []byte, c *Commit) []byte {
	dst = append(dst, "tree "...)
	dst = c.Tree.AppendHex(dst)
	dst = append(dst, '\n')
	for _, p := range c.Parents {
		dst = append(dst, "parent "...)
		dst = p.AppendHex(dst)
		dst = append(dst, '\n')
	}
	dst = appendSignature(dst, "author", &c.Author)
	dst = appendSignature(dst, "committer", &c.Committer)
	dst = append(dst, '\n')
	return append(dst, c.Message...)
}

// ParseCommit decodes a commit object. Unknown headers (gpgsig, encoding, mergetag and their
// continuation lines) are skipped.
func ParseCommit(data []byte) (Commit, error) {
	var c Commit
	seenTree := false
	for {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			// A header that runs to the end of the object: no message.
			end = len(data)
		}
		line := data[:end]
		if len(line) == 0 {
			if end < len(data) {
				c.Message = string(data[end+1:])
			}
			break
		}
		key, val, _ := bytes.Cut(line, []byte{' '})
		switch string(key) {
		case "tree":
			id, ok := ParseOIDBytes(val)
			if !ok {
				return c, ErrMalformed
			}
			c.Tree, seenTree = id, true
		case "parent":
			id, ok := ParseOIDBytes(val)
			if !ok {
				return c, ErrMalformed
			}
			c.Parents = append(c.Parents, id)
		case "author":
			c.Author = parseSignature(val)
		case "committer":
			c.Committer = parseSignature(val)
		}
		if end == len(data) {
			break
		}
		data = data[end+1:]
	}
	if !seenTree {
		return c, ErrMalformed
	}
	return c, nil
}

// parseSignature decodes "Name <email> seconds zone". Missing parts stay empty.
func parseSignature(b []byte) Signature {
	var s Signature
	lt := bytes.IndexByte(b, '<')
	gt := bytes.LastIndexByte(b, '>')
	if lt < 0 || gt < lt {
		s.Name = string(bytes.TrimSpace(b))
		return s
	}
	s.Name = string(bytes.TrimSpace(b[:lt]))
	s.Email = string(b[lt+1 : gt])
	when, zone, _ := bytes.Cut(bytes.TrimSpace(b[gt+1:]), []byte{' '})
	s.When, _ = strconv.ParseInt(string(when), 10, 64)
	s.Zone = string(zone)
	return s
}

// ParseTag reads the target, target type and name of an annotated tag.
func ParseTag(data []byte) (target OID, typ Type, name string, ok bool) {
	for len(data) > 0 && data[0] != '\n' {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			end = len(data)
		}
		key, val, _ := bytes.Cut(data[:end], []byte{' '})
		switch string(key) {
		case "object":
			if target, ok = ParseOIDBytes(val); !ok {
				return Zero, TypeNone, "", false
			}
		case "type":
			typ = ParseType(val)
		case "tag":
			name = string(val)
		}
		if end == len(data) {
			break
		}
		data = data[end+1:]
	}
	return target, typ, name, ok && typ != TypeNone
}

// Subject returns the first line of a commit message.
func Subject(message string) string {
	for i := 0; i < len(message); i++ {
		if message[i] == '\n' {
			return message[:i]
		}
	}
	return message
}
