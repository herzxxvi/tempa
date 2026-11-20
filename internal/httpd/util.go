package httpd

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
)

// Auth is the outcome of checking a request's credentials.
type Auth uint8

const (
	// AuthNone: the request has no Authorization header.
	AuthNone Auth = iota
	// AuthBad: the header is present but does not carry the server's token.
	AuthBad
	// AuthOK: the request carries the server's token.
	AuthOK
)

// Auth checks the Authorization header against the server's token. It accepts "Bearer <token>" and
// "token <token>" (scheme in any case) and, for git clients, Basic credentials whose password is the
// token. Every surface decides for itself what an AuthNone or AuthBad reply looks like.
func (c *Ctx) Auth() Auth {
	v := c.Authorization
	if bytes.Equal(v, c.srv.bearer) && c.srv.Token != "" {
		return AuthOK // the exact bytes every suite client sends
	}
	if len(v) == 0 {
		return AuthNone
	}
	sp := bytes.IndexByte(v, ' ')
	if sp < 0 {
		return AuthBad
	}
	scheme, cred := v[:sp], trim(v[sp+1:])
	switch {
	case equalFold(scheme, "bearer"), equalFold(scheme, "token"):
	case equalFold(scheme, "basic"):
		raw, err := base64.StdEncoding.AppendDecode(nil, cred)
		if err != nil {
			return AuthBad
		}
		if colon := bytes.IndexByte(raw, ':'); colon >= 0 {
			cred = raw[colon+1:]
		} else {
			cred = raw
		}
	default:
		return AuthBad
	}
	if len(cred) == 0 {
		return AuthBad
	}
	if c.srv.Token == "" || subtle.ConstantTimeCompare(cred, []byte(c.srv.Token)) == 1 {
		return AuthOK
	}
	return AuthBad
}

// ParseInt parses a non-negative decimal integer made of digits only. ok is false for an empty
// input, any other character, or a value that does not fit in an int64.
func ParseInt(b []byte) (n int64, ok bool) {
	if len(b) == 0 || len(b) > 18 {
		return 0, false
	}
	for _, ch := range b {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int64(ch-'0')
	}
	return n, true
}

// Unescape appends the percent-decoded form of src to dst. With plus set, '+' decodes to a space
// (query strings). ok is false when an escape is malformed; dst is then returned unchanged in length.
func Unescape(dst, src []byte, plus bool) (out []byte, ok bool) {
	mark := len(dst)
	for i := 0; i < len(src); i++ {
		ch := src[i]
		switch {
		case ch == '%':
			if i+2 >= len(src) {
				return dst[:mark], false
			}
			hi, lo := hexValue(src[i+1]), hexValue(src[i+2])
			if hi < 0 || lo < 0 {
				return dst[:mark], false
			}
			dst = append(dst, byte(hi<<4|lo))
			i += 2
		case ch == '+' && plus:
			dst = append(dst, ' ')
		default:
			dst = append(dst, ch)
		}
	}
	return dst, true
}

// NeedsUnescape reports whether a path segment contains a percent escape.
func NeedsUnescape(b []byte) bool { return bytes.IndexByte(b, '%') >= 0 }

// QueryRaw returns the raw (still encoded) value of the first query parameter called key. found is
// false when the parameter is absent; a parameter without '=' has an empty value.
func (c *Ctx) QueryRaw(key string) (value []byte, found bool) {
	q := c.Query
	for len(q) > 0 {
		pair := q
		if i := bytes.IndexByte(q, '&'); i >= 0 {
			pair, q = q[:i], q[i+1:]
		} else {
			q = nil
		}
		name := pair
		var val []byte
		if eq := bytes.IndexByte(pair, '='); eq >= 0 {
			name, val = pair[:eq], pair[eq+1:]
		}
		if string(name) == key {
			return val, true
		}
	}
	return nil, false
}

// QueryString returns the decoded value of a query parameter, or "" when it is absent or malformed.
func (c *Ctx) QueryString(key string) string {
	raw, ok := c.QueryRaw(key)
	if !ok || len(raw) == 0 {
		return ""
	}
	if bytes.IndexByte(raw, '%') < 0 && bytes.IndexByte(raw, '+') < 0 {
		return string(raw)
	}
	var buf [64]byte
	dec, ok := Unescape(buf[:0], raw, true)
	if !ok {
		return ""
	}
	return string(dec)
}

// QueryInt returns a query parameter as a non-negative integer, or def when it is absent or not a
// plain decimal number.
func (c *Ctx) QueryInt(key string, def int) int {
	raw, ok := c.QueryRaw(key)
	if !ok {
		return def
	}
	n, ok := ParseInt(raw)
	if !ok || n > 1<<31 {
		return def
	}
	return int(n)
}
