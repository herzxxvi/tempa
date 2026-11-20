package graphql

import (
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type tokenKind uint8

const (
	tokEOF    tokenKind = iota
	tokPunct            // ! $ & ( ) ... : = @ [ ] { | }
	tokName             // [_A-Za-z][_0-9A-Za-z]*
	tokInt              // text is the number as written
	tokFloat            // text is the number as written
	tokString           // text is the decoded value of a quoted or block string
)

type token struct {
	kind tokenKind
	text string
	line int // 1-based
	col  int // 1-based, counted in characters
}

// lexer turns GraphQL source text into tokens. Spaces, tabs, line ends, commas, a byte order mark
// and # comments separate tokens and are otherwise ignored. Lexical errors are raised by panicking
// with an *Error; the parser's entry points recover them.
type lexer struct {
	src       string
	pos       int
	line      int // line of pos
	lineStart int // offset of the first byte of that line
}

func newLexer(src string) lexer { return lexer{src: src, line: 1} }

func (l *lexer) column(pos int) int {
	return utf8.RuneCountInString(l.src[l.lineStart:pos]) + 1
}

func (l *lexer) fail(pos int, message string) {
	col := l.column(pos)
	panic(&Error{
		Message: message + " at [" + strconv.Itoa(l.line) + ", " + strconv.Itoa(col) + "]",
		Line:    l.line, Col: col,
	})
}

// lineEnd consumes the line terminator at pos (\n, \r\n or \r) and starts a new line.
func (l *lexer) lineEnd() {
	if l.src[l.pos] == '\r' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '\n' {
		l.pos++
	}
	l.pos++
	l.line++
	l.lineStart = l.pos
}

func (l *lexer) skipIgnored() {
	for l.pos < len(l.src) {
		switch l.src[l.pos] {
		case ' ', '\t', ',':
			l.pos++
		case '\n', '\r':
			l.lineEnd()
		case '#':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' && l.src[l.pos] != '\r' {
				l.pos++
			}
		case 0xEF:
			if !strings.HasPrefix(l.src[l.pos:], "\xEF\xBB\xBF") {
				return
			}
			if l.pos == l.lineStart {
				l.lineStart += 3 // a byte order mark takes no column
			}
			l.pos += 3
		default:
			return
		}
	}
}

func isNameStart(ch byte) bool {
	return ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z'
}

func isDigit(ch byte) bool { return ch >= '0' && ch <= '9' }

func (l *lexer) next() token {
	l.skipIgnored()
	start := l.pos
	t := token{line: l.line, col: l.column(start)}
	if start >= len(l.src) {
		return t
	}
	switch ch := l.src[start]; {
	case isNameStart(ch):
		for l.pos < len(l.src) && (isNameStart(l.src[l.pos]) || isDigit(l.src[l.pos])) {
			l.pos++
		}
		t.kind, t.text = tokName, l.src[start:l.pos]
	case ch == '-' || isDigit(ch):
		t.kind = l.number()
		t.text = l.src[start:l.pos]
	case ch == '"':
		t.kind = tokString
		if strings.HasPrefix(l.src[start:], `"""`) {
			t.text = l.blockString()
		} else {
			t.text = l.quotedString()
		}
	case ch == '.':
		if !strings.HasPrefix(l.src[start:], "...") {
			l.fail(start, `Parse error on "." (error)`)
		}
		l.pos += 3
		t.kind, t.text = tokPunct, "..."
	case strings.IndexByte("!$&():=@[]{|}", ch) >= 0:
		l.pos++
		t.kind, t.text = tokPunct, l.src[start:l.pos]
	default:
		r, _ := utf8.DecodeRuneInString(l.src[start:])
		l.fail(start, "Parse error on "+strconv.Quote(string(r))+" (error)")
	}
	return t
}

func (l *lexer) digits() {
	if l.pos >= len(l.src) || !isDigit(l.src[l.pos]) {
		l.fail(l.pos, "Parse error on a malformed number (error)")
	}
	for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
		l.pos++
	}
}

// number scans -?(0|[1-9][0-9]*) with an optional fraction and exponent, which make it a float.
func (l *lexer) number() tokenKind {
	if l.src[l.pos] == '-' {
		l.pos++
	}
	if l.pos < len(l.src) && l.src[l.pos] == '0' {
		l.pos++
		if l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.fail(l.pos, "Parse error on a number with a leading zero (error)")
		}
	} else {
		l.digits()
	}
	kind := tokInt
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		l.pos++
		l.digits()
		kind = tokFloat
	}
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.pos++
		}
		l.digits()
		kind = tokFloat
	}
	if l.pos < len(l.src) && (isNameStart(l.src[l.pos]) || l.src[l.pos] == '.') {
		l.fail(l.pos, "Parse error on a malformed number (error)")
	}
	return kind
}

// quotedString scans "..." and returns its value with the escapes decoded.
func (l *lexer) quotedString() string {
	open := l.pos
	l.pos++
	var buf []byte
	escaped := false
	start := l.pos
	for l.pos < len(l.src) {
		switch l.src[l.pos] {
		case '"':
			s := l.src[start:l.pos]
			l.pos++
			if escaped {
				return string(append(buf, s...))
			}
			return s
		case '\n', '\r':
			l.fail(open, "Parse error on an unterminated string (error)")
		case '\\':
			buf = append(buf, l.src[start:l.pos]...)
			l.pos++
			buf = l.escape(buf)
			escaped = true
			start = l.pos
		default:
			l.pos++
		}
	}
	l.fail(open, "Parse error on an unterminated string (error)")
	return ""
}

func (l *lexer) escape(buf []byte) []byte {
	if l.pos >= len(l.src) {
		l.fail(l.pos-1, "Parse error on an unterminated string (error)")
	}
	ch := l.src[l.pos]
	l.pos++
	switch ch {
	case '"', '\\', '/':
		return append(buf, ch)
	case 'b':
		return append(buf, '\b')
	case 'f':
		return append(buf, '\f')
	case 'n':
		return append(buf, '\n')
	case 'r':
		return append(buf, '\r')
	case 't':
		return append(buf, '\t')
	case 'u':
		r := l.codePoint()
		if utf16.IsSurrogate(r) {
			// A surrogate pair written as two \uXXXX escapes is one character.
			if strings.HasPrefix(l.src[l.pos:], `\u`) {
				l.pos += 2
				r = utf16.DecodeRune(r, l.codePoint())
			} else {
				r = utf8.RuneError
			}
		}
		return utf8.AppendRune(buf, r)
	}
	l.fail(l.pos-2, "Parse error on a bad escape sequence in a string (error)")
	return buf
}

// codePoint scans the XXXX of \uXXXX or the {X...} of \u{X...}.
func (l *lexer) codePoint() rune {
	start := l.pos
	end := start + 4
	if l.pos < len(l.src) && l.src[l.pos] == '{' {
		start++
		n := strings.IndexByte(l.src[start:], '}')
		if n < 1 || n > 6 {
			l.fail(l.pos, "Parse error on a bad Unicode escape sequence (error)")
		}
		end = start + n
		l.pos = end + 1
	} else {
		l.pos = end
	}
	if end > len(l.src) {
		l.fail(start, "Parse error on a bad Unicode escape sequence (error)")
	}
	v, err := strconv.ParseUint(l.src[start:end], 16, 32)
	if err != nil || v > utf8.MaxRune {
		l.fail(start, "Parse error on a bad Unicode escape sequence (error)")
	}
	return rune(v)
}

// blockString scans """...""" and returns its value: \""" stands for three quotes, the common
// indentation is removed and blank first and last lines are dropped.
func (l *lexer) blockString() string {
	open, line, lineStart := l.pos, l.line, l.lineStart
	l.pos += 3
	var raw []byte
	start := l.pos
	for l.pos < len(l.src) {
		switch ch := l.src[l.pos]; {
		case ch == '"' && strings.HasPrefix(l.src[l.pos:], `"""`):
			raw = append(raw, l.src[start:l.pos]...)
			l.pos += 3
			return blockStringValue(string(raw))
		case ch == '\\' && strings.HasPrefix(l.src[l.pos:], `\"""`):
			raw = append(raw, l.src[start:l.pos]...)
			raw = append(raw, `"""`...)
			l.pos += 4
			start = l.pos
		case ch == '\n' || ch == '\r':
			l.lineEnd()
		default:
			l.pos++
		}
	}
	l.pos, l.line, l.lineStart = open, line, lineStart
	l.fail(open, "Parse error on an unterminated block string (error)")
	return ""
}

func isBlank(s string) bool { return strings.Trim(s, " \t") == "" }

// blockStringValue is the BlockStringValue algorithm of the GraphQL specification.
func blockStringValue(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")
	indent := -1
	for _, line := range lines[1:] {
		if n := len(line) - len(strings.TrimLeft(line, " \t")); n < len(line) && (indent < 0 || n < indent) {
			indent = n
		}
	}
	if indent > 0 {
		for i := 1; i < len(lines); i++ {
			lines[i] = lines[i][min(indent, len(lines[i])):]
		}
	}
	for len(lines) > 0 && isBlank(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && isBlank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}
