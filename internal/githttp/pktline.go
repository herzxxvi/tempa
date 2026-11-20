package githttp

// pkt-line framing: four hex digits giving the length of the line including those four bytes,
// then the payload. "0000" is a flush, "0001" a delimiter, "0002" a response end.

const (
	maxPkt      = 65520      // largest pkt-line, header included
	maxBandData = maxPkt - 5 // payload of a side-band packet after its length and band byte
)

const hexLower = "0123456789abcdef"

// putHex4 writes n as four lowercase hex digits at the start of b.
func putHex4(b []byte, n int) {
	b[0] = hexLower[n>>12&15]
	b[1] = hexLower[n>>8&15]
	b[2] = hexLower[n>>4&15]
	b[3] = hexLower[n&15]
}

// beginPkt reserves the length field of a pkt-line and returns its position; endPkt fills it in
// once the payload has been appended.
func beginPkt(dst []byte) ([]byte, int) {
	return append(dst, "0000"...), len(dst)
}

func endPkt(dst []byte, mark int) []byte {
	putHex4(dst[mark:], len(dst)-mark)
	return dst
}

// appendPkt appends one pkt-line carrying payload.
func appendPkt(dst []byte, payload string) []byte {
	dst, mark := beginPkt(dst)
	dst = append(dst, payload...)
	return endPkt(dst, mark)
}

const (
	flushPkt = "0000"
	delimPkt = "0001"
)

// Kinds of pkt-line a reader returns.
const (
	pktData = iota
	pktFlush
	pktDelim
	pktEnd   // "0002"
	pktEOF   // no bytes left
	pktError // malformed: bad length digits, length 3, or a length past the end of the input
)

// pktReader walks the pkt-lines of a fully buffered request body. It never waits for more bytes.
type pktReader struct {
	buf []byte
	pos int
}

// next returns the next pkt-line. For pktData, payload is a view into the buffer.
func (p *pktReader) next() (payload []byte, kind int) {
	rest := p.buf[p.pos:]
	if len(rest) == 0 {
		return nil, pktEOF
	}
	if len(rest) < 4 {
		return nil, pktError
	}
	n := 0
	for i := 0; i < 4; i++ {
		ch := rest[i]
		var v int
		switch {
		case ch >= '0' && ch <= '9':
			v = int(ch - '0')
		case ch >= 'a' && ch <= 'f':
			v = int(ch-'a') + 10
		case ch >= 'A' && ch <= 'F':
			v = int(ch-'A') + 10
		default:
			return nil, pktError
		}
		n = n<<4 | v
	}
	switch {
	case n == 0:
		p.pos += 4
		return nil, pktFlush
	case n == 1:
		p.pos += 4
		return nil, pktDelim
	case n == 2:
		p.pos += 4
		return nil, pktEnd
	case n < 4, n > maxPkt, n > len(rest):
		return nil, pktError
	}
	p.pos += n
	return rest[4:n], pktData
}

// rest returns the bytes after the last pkt-line read (the pack that follows push commands).
func (p *pktReader) rest() []byte { return p.buf[p.pos:] }

// chomp removes one trailing newline.
func chomp(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\n' {
		return line[:n-1]
	}
	return line
}

// cutPrefix reports whether line starts with prefix and returns what follows it.
func cutPrefix(line []byte, prefix string) ([]byte, bool) {
	if len(line) >= len(prefix) && string(line[:len(prefix)]) == prefix {
		return line[len(prefix):], true
	}
	return nil, false
}

// appendSideband wraps data in band-1 side-band packets of at most maxPkt bytes each.
func appendSideband(dst, data []byte) []byte {
	for len(data) > 0 {
		n := min(len(data), maxBandData)
		var mark int
		dst, mark = beginPkt(dst)
		dst = append(dst, 1)
		dst = append(dst, data[:n]...)
		dst = endPkt(dst, mark)
		data = data[n:]
	}
	return dst
}
