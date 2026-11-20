// Package render turns store objects into the JSON the REST API returns. There is exactly one
// renderer per object type, and REST, MCP and anything else that shows a REST object all call it,
// so an object reads back byte for byte the same on every surface.
//
// The renderers are hand-written append functions with a fixed key order: no reflection, no maps,
// no intermediate structures, and no allocation beyond growing the destination. Lists must start
// "[" and never render null, strings are escaped by AppendString only, and nothing here adds a key
// "for fidelity" that no client reads: every byte is parsed again on the other side.
//
// Absolute URLs are built from the request's Host header ("http://" + host), which is the origin
// the client itself used, so links (and the Link header of paginated lists) stay on that origin.
package render

import (
	"strconv"
	"unicode/utf8"

	"forge/internal/gitobj"
)

const hexUpper = "0123456789ABCDEF"

// plainASCII[ch] is true for the ASCII bytes a JSON string holds unescaped: everything from the
// space up except the quote and the backslash. Text is almost entirely such bytes, so AppendString
// tests one table entry per byte and only looks closer at the rest.
var plainASCII = func() (t [256]bool) {
	for ch := 0x20; ch < utf8.RuneSelf; ch++ {
		t[ch] = ch != '"' && ch != '\\'
	}
	return t
}()

// AppendString appends s as a JSON string. It escapes the quote, the backslash and control
// characters, replaces invalid UTF-8 with U+FFFD and passes everything else through unchanged; in
// particular '<', '>' and '&' are not escaped, so text round-trips byte for byte.
func AppendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		ch := s[i]
		if plainASCII[ch] {
			i++
			continue
		}
		if ch < utf8.RuneSelf {
			dst = append(dst, s[start:i]...)
			switch ch {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexUpper[ch>>4], hexUpper[ch&15])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, `�`...)
			i++
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// appendKey appends `,"key":` (or `"key":` right after an opening brace).
func appendKey(dst []byte, key string) []byte {
	if n := len(dst); n > 0 && dst[n-1] != '{' {
		dst = append(dst, ',')
	}
	dst = append(dst, '"')
	dst = append(dst, key...)
	return append(dst, '"', ':')
}

func appendStr(dst []byte, key, value string) []byte {
	return AppendString(appendKey(dst, key), value)
}

func appendInt(dst []byte, key string, value int64) []byte {
	return strconv.AppendInt(appendKey(dst, key), value, 10)
}

func appendBool(dst []byte, key string, value bool) []byte {
	return strconv.AppendBool(appendKey(dst, key), value)
}

func appendNull(dst []byte, key string) []byte {
	return append(appendKey(dst, key), "null"...)
}

// appendNullable appends a string value, or null when present is false.
func appendNullable(dst []byte, key, value string, present bool) []byte {
	if !present {
		return appendNull(dst, key)
	}
	return appendStr(dst, key, value)
}

// appendSHA appends an object id as a JSON string value under key.
func appendSHA(dst []byte, key string, id gitobj.OID) []byte {
	dst = append(appendKey(dst, key), '"')
	dst = id.AppendHex(dst)
	return append(dst, '"')
}

// appendTimeKey appends a timestamp under key, or null for the zero time.
func appendTimeKey(dst []byte, key string, t int64) []byte {
	dst = appendKey(dst, key)
	if t == 0 {
		return append(dst, "null"...)
	}
	dst = append(dst, '"')
	dst = AppendTime(dst, t)
	return append(dst, '"')
}

// AppendTime appends t (seconds since the Unix epoch, not negative) as "2006-01-02T15:04:05Z".
func AppendTime(dst []byte, t int64) []byte {
	days, secs := t/86400, t%86400
	// Civil date from a day count: shift the epoch to 0000-03-01 so leap days fall at the end of
	// the year, then peel off 400-year eras, years within the era and months of the shifted year.
	z := days + 719468
	era := z / 146097
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	day := doy - (153*mp+2)/5 + 1
	month := mp + 3
	year := yoe + era*400
	if month > 12 {
		month -= 12
		year++
	}
	dst = append4(dst, year)
	dst = append(dst, '-')
	dst = append2(dst, month)
	dst = append(dst, '-')
	dst = append2(dst, day)
	dst = append(dst, 'T')
	dst = append2(dst, secs/3600)
	dst = append(dst, ':')
	dst = append2(dst, secs%3600/60)
	dst = append(dst, ':')
	dst = append2(dst, secs%60)
	return append(dst, 'Z')
}

func append2(dst []byte, v int64) []byte {
	return append(dst, byte('0'+v/10%10), byte('0'+v%10))
}

func append4(dst []byte, v int64) []byte {
	return append(dst, byte('0'+v/1000%10), byte('0'+v/100%10), byte('0'+v/10%10), byte('0'+v%10))
}

// appendOrigin appends "http://<host>".
func appendOrigin(dst, host []byte) []byte {
	dst = append(dst, "http://"...)
	return append(dst, host...)
}

// appendURLPath appends a slash-separated path with everything outside the unreserved set
// percent-encoded, slashes kept. The result is also safe inside a JSON string.
func appendURLPath(dst []byte, path string) []byte {
	for i := 0; i < len(path); i++ {
		ch := path[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '-', ch == '.', ch == '_', ch == '~', ch == '/':
			dst = append(dst, ch)
		default:
			dst = append(dst, '%', hexUpper[ch>>4], hexUpper[ch&15])
		}
	}
	return dst
}
