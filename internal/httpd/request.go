package httpd

import "bytes"

// Method is the request method as a small integer, usable as an array index.
type Method uint8

const (
	OTHER Method = iota
	GET
	HEAD
	POST
	PUT
	PATCH
	DELETE
	OPTIONS
	numMethods
)

type header struct{ name, value []byte }

// Ctx is one connection's request/response state. The engine hands the same Ctx to the handler for
// every request on the connection; request fields are views into the input buffer and die when the
// handler returns. Copy anything that must outlive the request.
type Ctx struct {
	// Method is the parsed request method; OTHER for anything not listed above.
	Method Method
	// Path is the request path, undecoded, without the query string.
	Path []byte
	// Query is the raw query string without the '?'; nil when there is none.
	Query []byte
	// Body is the complete request body (already de-chunked); empty when there is none.
	Body []byte

	// The header values every surface needs, trimmed, nil when absent. Other headers: Header.
	Host          []byte
	Authorization []byte
	ContentType   []byte
	Accept        []byte
	GitProtocol   []byte

	// Scratch is a buffer the handler may use for building a response body. The engine never
	// touches it; it survives between requests so its capacity is reused.
	Scratch []byte

	srv     *Server
	hdrs    []header
	params  [maxParams][]byte
	nparams int

	// Response state for the current request.
	out        []byte // responses not yet written
	extra      []byte // extra header lines for the current reply
	replied    bool
	closeAfter bool // close the connection once the pending responses are written
	isHead     bool

	// Connection buffers. in[start:end] holds bytes read but not yet consumed.
	in           []byte
	start, end   int
	scanned      int    // bytes of the pending request already searched for the end of the headers
	need         int    // total size of the pending request once its headers announced it; 0 = unknown
	chunked      []byte // decoded chunked body, complete or (with chunkPos) in progress
	chunkPos     int    // how far into the encoded body of the pending request decoding has come
	wantContinue bool   // the pending request sent "Expect: 100-continue" and its body is incomplete
	sent100      bool

	// Used by the epoll loop only.
	fd      int  // the connection's descriptor
	written int  // bytes of out already sent
	blocked bool // the socket would not take all of out: waiting for it to become writable
}

func (c *Ctx) resetConn() {
	c.start, c.end, c.scanned, c.need = 0, 0, 0, 0
	c.out = c.out[:0]
	c.closeAfter, c.wantContinue, c.sent100, c.replied = false, false, false, false
	c.written, c.blocked = 0, false
	c.chunked, c.chunkPos = c.chunked[:0], 0
}

// grow makes the input buffer at least size bytes long, keeping what it holds. Small buffers
// double; a large one is sized to what is needed, so a body of n bytes costs n, not up to 2n.
func (c *Ctx) grow(size int) {
	n := len(c.in)
	if size > growExact {
		n = (size + 4095) &^ 4095
	} else {
		for n < size {
			n *= 2
		}
	}
	bigger := make([]byte, n)
	copy(bigger, c.in[:c.end])
	c.in = bigger
}

// room makes sure there is space to read more of the pending request: enough for the whole
// request when its headers announced a length, otherwise up to twice the current buffer. It reports
// false when the buffer is full at maxRequestSize, i.e. the request has outgrown what is accepted.
func (c *Ctx) room() bool {
	switch {
	case c.need > len(c.in):
		c.grow(c.need)
	case c.end < len(c.in):
	case len(c.in) >= maxRequestSize:
		return false
	default:
		c.grow(min(2*len(c.in), maxRequestSize))
	}
	return true
}

// shrink replaces buffers that one large request left oversized. It runs only when no request is
// pending, so nothing still points into them.
func (c *Ctx) shrink() {
	if len(c.in) > keepBuffer {
		c.in = make([]byte, inputSize)
	}
	if cap(c.out) > keepBuffer {
		c.out = make([]byte, 0, outputSize)
	}
	if cap(c.chunked) > keepBuffer {
		c.chunked = nil
	}
}

// Header returns the value of a request header, or nil. name must be lowercase.
func (c *Ctx) Header(name string) []byte {
	for i := range c.hdrs {
		if equalFold(c.hdrs[i].name, name) {
			return c.hdrs[i].value
		}
	}
	return nil
}

// needMore is parse's "the request is not complete yet" result.
const needMore = -1

// maxBlankLines is how many bytes of empty lines may precede a request line.
const maxBlankLines = 64

// parse decodes the request at the front of buf. It returns the number of bytes the request
// occupies and 0, or needMore, or the HTTP status of the error reply to send before closing.
func (c *Ctx) parse(buf []byte) (used int, code int) {
	// Tolerate empty lines before the request line.
	skip := 0
	for skip < len(buf) && (buf[skip] == '\r' || buf[skip] == '\n') {
		skip++
	}
	if skip > maxBlankLines {
		return 0, 400
	}
	from := max(skip, c.scanned-3)
	i := bytes.Index(buf[from:], crlfcrlf)
	if i < 0 {
		if len(buf)-skip > maxHeaderSize {
			return 0, 431
		}
		c.scanned = len(buf)
		return 0, needMore
	}
	headEnd := from + i + 4
	if headEnd-skip > maxHeaderSize {
		return 0, 431
	}
	head := buf[skip : headEnd-2] // request line and header lines, each ending in CRLF

	// Request line: METHOD SP target SP HTTP/1.x
	eol := bytes.Index(head, crlf)
	line := head[:eol]
	sp1 := bytes.IndexByte(line, ' ')
	sp2 := bytes.LastIndexByte(line, ' ')
	if sp1 <= 0 || sp2 <= sp1+1 {
		return 0, 400
	}
	http10 := false
	switch string(line[sp2+1:]) {
	case "HTTP/1.1":
	case "HTTP/1.0":
		http10 = true
	default:
		if bytes.HasPrefix(line[sp2+1:], []byte("HTTP/")) {
			return 0, 505
		}
		return 0, 400
	}
	c.Method = parseMethod(line[:sp1])
	c.isHead = c.Method == HEAD
	target := line[sp1+1 : sp2]
	if target[0] != '/' {
		// absolute-form ("http://host/path") or "*"; anything else is malformed.
		if j := bytes.Index(target, []byte("://")); j > 0 {
			rest := target[j+3:]
			if k := bytes.IndexByte(rest, '/'); k >= 0 {
				target = rest[k:]
			} else {
				target = slash
			}
		} else if string(target) != "*" {
			return 0, 400
		}
	}
	if q := bytes.IndexByte(target, '?'); q >= 0 {
		c.Path, c.Query = target[:q], target[q+1:]
	} else {
		c.Path, c.Query = target, nil
	}

	// Headers.
	c.hdrs = c.hdrs[:0]
	c.Host, c.Authorization, c.ContentType, c.Accept, c.GitProtocol = nil, nil, nil, nil, nil
	length := -1
	chunked, expect := false, false
	keepAlive, closeAsked := false, false
	for rest := head[eol+2:]; len(rest) > 0; {
		eol = bytes.Index(rest, crlf)
		line, rest = rest[:eol], rest[eol+2:]
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 || line[colon-1] == ' ' || line[colon-1] == '\t' || line[0] == ' ' || line[0] == '\t' {
			return 0, 400
		}
		name, value := line[:colon], trim(line[colon+1:])
		c.hdrs = append(c.hdrs, header{name, value})
		switch len(name) {
		case 4:
			if equalFold(name, "host") {
				c.Host = value
			}
		case 6:
			if equalFold(name, "accept") {
				c.Accept = value
			} else if equalFold(name, "expect") {
				expect = equalFold(value, "100-continue")
			}
		case 10:
			if equalFold(name, "connection") {
				closeAsked = closeAsked || hasToken(value, "close")
				keepAlive = keepAlive || hasToken(value, "keep-alive")
			}
		case 12:
			if equalFold(name, "content-type") {
				c.ContentType = value
			} else if equalFold(name, "git-protocol") {
				c.GitProtocol = value
			}
		case 13:
			if equalFold(name, "authorization") {
				c.Authorization = value
			}
		case 14:
			if equalFold(name, "content-length") {
				n, ok := ParseInt(value)
				if !ok || (length >= 0 && int64(length) != n) {
					return 0, 400
				}
				if n > maxBodySize {
					return 0, 413
				}
				length = int(n)
			}
		case 17:
			if equalFold(name, "transfer-encoding") {
				if !hasToken(value, "chunked") {
					return 0, 501
				}
				chunked = true
			}
		}
	}
	if !validHost(c.Host) {
		c.Host = c.srv.defaultHost
	}

	// Body.
	used = headEnd
	c.Body = nil
	switch {
	case chunked:
		// Decoding resumes where the previous attempt stopped, so a large upload that arrives in
		// many reads is decoded once, not once per read.
		body, n, code := dechunk(c.chunked, buf[headEnd:], c.chunkPos)
		if code != 0 {
			if code == needMore {
				c.chunked, c.chunkPos = body, n
				c.wantContinue = expect
			}
			return 0, code
		}
		c.chunked, c.chunkPos = body[:0], 0
		c.Body = body
		used += n
	case length > 0:
		if len(buf)-headEnd < length {
			c.need = headEnd + length
			c.wantContinue = expect
			return 0, needMore
		}
		c.Body = buf[headEnd : headEnd+length]
		used += length
	}
	// Only a complete request may ask for the connection to be closed after its response.
	if closeAsked || (http10 && !keepAlive) {
		c.closeAfter = true
	}
	return used, 0
}

var (
	crlf     = []byte("\r\n")
	crlfcrlf = []byte("\r\n\r\n")
	slash    = []byte("/")
)

// validHost reports whether a Host header value is a plain host[:port]. Handlers paste the host
// into URLs inside JSON and headers, so anything else is replaced by the server's default host.
func validHost(h []byte) bool {
	if len(h) == 0 || len(h) > 255 {
		return false
	}
	for _, ch := range h {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '.', ch == '-', ch == ':', ch == '[', ch == ']', ch == '_':
		default:
			return false
		}
	}
	return true
}

func parseMethod(m []byte) Method {
	switch string(m) {
	case "GET":
		return GET
	case "POST":
		return POST
	case "PUT":
		return PUT
	case "PATCH":
		return PATCH
	case "DELETE":
		return DELETE
	case "HEAD":
		return HEAD
	case "OPTIONS":
		return OPTIONS
	}
	return OTHER
}

// dechunk decodes a chunked body at the front of buf, appending the payload to dst. It starts at
// offset from, which must be a chunk boundary (0, or the n of an earlier call that needed more).
// code is 0, needMore or an HTTP error status. With 0, n is the number of bytes of buf the encoded
// body occupies; with needMore, n is the boundary up to which body holds the decoded payload, to be
// passed back as from once more bytes have arrived.
func dechunk(dst, buf []byte, from int) (body []byte, n int, code int) {
	pos := from
	for {
		mark := pos
		eol := bytes.Index(buf[pos:], crlf)
		if eol < 0 {
			if len(buf)-pos > 4096 {
				return dst, 0, 400
			}
			return dst, mark, needMore
		}
		line := buf[pos : pos+eol]
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = line[:semi] // chunk extensions are ignored
		}
		line = trim(line)
		if len(line) == 0 || len(line) > 8 {
			return dst, 0, 400
		}
		size := 0
		for _, ch := range line {
			v := hexValue(ch)
			if v < 0 {
				return dst, 0, 400
			}
			size = size<<4 | v
		}
		pos += eol + 2
		if size == 0 {
			// Trailer section: header lines up to an empty line.
			for {
				eol = bytes.Index(buf[pos:], crlf)
				if eol < 0 {
					if len(buf)-pos > maxHeaderSize {
						return dst, 0, 400
					}
					return dst, mark, needMore
				}
				pos += eol + 2
				if eol == 0 {
					return dst, pos, 0
				}
			}
		}
		if len(dst)+size > maxBodySize {
			return dst, 0, 413
		}
		if len(buf)-pos < size+2 {
			return dst, mark, needMore
		}
		dst = append(dst, buf[pos:pos+size]...)
		pos += size
		if buf[pos] != '\r' || buf[pos+1] != '\n' {
			return dst, 0, 400
		}
		pos += 2
	}
}

func hexValue(ch byte) int {
	switch {
	case ch >= '0' && ch <= '9':
		return int(ch - '0')
	case ch >= 'a' && ch <= 'f':
		return int(ch-'a') + 10
	case ch >= 'A' && ch <= 'F':
		return int(ch-'A') + 10
	}
	return -1
}

// trim removes leading and trailing spaces and tabs.
func trim(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}

// equalFold reports whether b equals lower, ignoring ASCII case. lower must be lowercase.
func equalFold(b []byte, lower string) bool {
	if len(b) != len(lower) {
		return false
	}
	for i := 0; i < len(b); i++ {
		ch := b[i]
		if ch != lower[i] && (ch < 'A' || ch > 'Z' || ch+32 != lower[i]) {
			return false
		}
	}
	return true
}

// hasToken reports whether a comma-separated header value contains token (lowercase), ignoring case.
func hasToken(value []byte, token string) bool {
	for len(value) > 0 {
		part := value
		if i := bytes.IndexByte(value, ','); i >= 0 {
			part, value = value[:i], value[i+1:]
		} else {
			value = nil
		}
		if equalFold(trim(part), token) {
			return true
		}
	}
	return false
}

// DecodeChunked decodes a complete chunked message body. ok is false while the body is incomplete
// or when it is malformed.
func DecodeChunked(buf []byte) (body []byte, ok bool) {
	body, _, code := dechunk(nil, buf, 0)
	return body, code == 0
}
