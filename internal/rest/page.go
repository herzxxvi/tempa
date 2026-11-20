package rest

import (
	"strconv"

	"forge/internal/httpd"
)

// Pagination. Every list route takes per_page (default 30, at most 100) and page (default 1) and
// answers a page past the end with an empty array. While more pages exist the reply carries
//
//	Link: <ORIGIN/api/v3/...?per_page=10&page=2>; rel="next", <...&page=6>; rel="last"
//
// where ORIGIN is "http://" + the request's Host header, so the links stay on the origin the client
// used, and the query is the request's own with only page replaced.

const (
	defaultPerPage = 30
	maxPerPage     = 100
)

// page is the window a list request asks for.
type page struct {
	number int // 1-based
	size   int
}

// paging reads per_page and page from the query. Values that are not plain numbers fall back to the
// defaults; per_page is capped at 100.
func paging(c *httpd.Ctx) page {
	p := page{number: c.QueryInt("page", 1), size: c.QueryInt("per_page", defaultPerPage)}
	if p.number < 1 {
		p.number = 1
	}
	if p.size < 1 {
		p.size = defaultPerPage
	}
	if p.size > maxPerPage {
		p.size = maxPerPage
	}
	return p
}

// offset is the number of items before the page.
func (p page) offset() int { return (p.number - 1) * p.size }

// bounds returns the page's slice bounds within a list of total items.
func (p page) bounds(total int) (lo, hi int) {
	lo = min(p.offset(), total)
	hi = min(lo+p.size, total)
	return lo, hi
}

// link sets the Link header for a list of total items: next and last while pages remain, prev and
// first after the first page. A single-page list gets no header.
func (p page) link(c *httpd.Ctx, total int) {
	last := (total + p.size - 1) / p.size
	p.setLink(c, p.number < last, last)
}

// linkMore is link for a list whose length is not known (commit history): next when more items
// follow, never last.
func (p page) linkMore(c *httpd.Ctx, more bool) { p.setLink(c, more, 0) }

func (p page) setLink(c *httpd.Ctx, next bool, last int) {
	if !next && p.number == 1 {
		return
	}
	var buf [512]byte
	b := buf[:0]
	if p.number > 1 {
		b = appendLink(b, c, p.number-1, "prev")
	}
	if next {
		b = appendLink(b, c, p.number+1, "next")
		if last > 0 {
			b = appendLink(b, c, last, "last")
		}
	}
	if p.number > 1 {
		b = appendLink(b, c, 1, "first")
	}
	c.AddHeaderBytes("Link", b)
}

// appendLink appends one `<url>; rel="name"` element: the request's own URL with page set to n.
func appendLink(b []byte, c *httpd.Ctx, n int, rel string) []byte {
	if len(b) > 0 {
		b = append(b, ", "...)
	}
	b = append(b, "<http://"...)
	b = append(b, c.Host...)
	b = appendLinkSafe(b, c.Path)
	sep := byte('?')
	for q := c.Query; len(q) > 0; {
		pair := q
		q = nil
		for i, ch := range pair {
			if ch == '&' {
				pair, q = pair[:i], pair[i+1:]
				break
			}
		}
		if len(pair) == 0 || string(pair) == "page" || (len(pair) > 5 && string(pair[:5]) == "page=") {
			continue
		}
		b = append(b, sep)
		b = appendLinkSafe(b, pair)
		sep = '&'
	}
	b = append(b, sep)
	b = append(b, "page="...)
	b = strconv.AppendInt(b, int64(n), 10)
	b = append(b, `>; rel="`...)
	b = append(b, rel...)
	return append(b, '"')
}

// appendLinkSafe appends a raw URL part, percent-encoding the characters that would end the URL or
// the element inside a Link header: clients split the header on commas and read up to '>'.
func appendLinkSafe(b, raw []byte) []byte {
	for _, ch := range raw {
		switch ch {
		case ',', '<', '>', '"', ' ', ';':
			b = append(b, '%', "0123456789ABCDEF"[ch>>4], "0123456789ABCDEF"[ch&15])
		default:
			b = append(b, ch)
		}
	}
	return b
}
