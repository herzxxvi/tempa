package graphql

import (
	"encoding/base64"
	"strings"
)

// Connections: every paged list of the schema (issues, pull requests, files, comments, labels,
// search results) is one listing, cut to a page by the same code.

// maxPage is the most items one page holds, as on GitHub.
const maxPage = 100

// listing is a complete result list in its final order.
type listing struct {
	items []any
	// key names an item inside a cursor; it must not change while the item lives.
	key func(item any) string
	// locate returns the range of positions [lo, hi) the item named by key occupies: the page
	// after it starts at hi, the page before it ends at lo. An item that has left the list since
	// the cursor was issued has an empty range where it would be now. ok is false for a key that
	// names nothing.
	locate func(key string) (lo, hi int, ok bool)
}

// identified builds a listing whose cursors are found again by comparing keys.
func identified(items []any, key func(item any) string) *listing {
	return &listing{items: items, key: key, locate: func(k string) (int, int, bool) {
		for i, item := range items {
			if key(item) == k {
				return i, i + 1, true
			}
		}
		return 0, 0, false
	}}
}

// ordered builds a listing whose order is the comparison cmp, which lets a cursor keep its place
// even when its item no longer matches the listing's filter. find returns the item a key names.
func ordered(items []any, key func(item any) string, find func(key string) any, cmp func(a, b any) int) *listing {
	return &listing{items: items, key: key, locate: func(k string) (lo, hi int, ok bool) {
		at := find(k)
		if at == nil {
			return 0, 0, false
		}
		for _, item := range items {
			switch c := cmp(item, at); {
			case c < 0:
				lo++
				hi++
			case c == 0:
				hi++
			}
		}
		return lo, hi, true
	}}
}

// connection is one page of a listing: the value of every *Connection type.
type connection struct {
	list    *listing
	page    []any
	total   int
	hasNext bool
	hasPrev bool
	counts  [3]int // search only: issues, repositories, users
}

// edge is the value of every *Edge type.
type edge struct {
	cursor string
	node   any
}

// pageInfo is the value of PageInfo.
type pageInfo struct {
	hasNext bool
	hasPrev bool
	start   any // cursor string, or nil for an empty page
	end     any
}

const cursorPrefix = "cursor:v2:"

func cursorOf(key string) string {
	return base64.StdEncoding.EncodeToString([]byte(cursorPrefix + key))
}

func (l *listing) position(cursor string) (lo, hi int, err error) {
	raw, decodeErr := base64.StdEncoding.DecodeString(cursor)
	key, ok := strings.CutPrefix(string(raw), cursorPrefix)
	if decodeErr == nil && ok {
		if lo, hi, ok = l.locate(key); ok {
			return lo, hi, nil
		}
	}
	return 0, 0, &Error{Type: "INVALID_CURSOR_ARGUMENTS", Message: "`" + cursor + "` does not appear to be a valid cursor."}
}

// paginate cuts the page selected by the first, after, last and before arguments out of a listing.
// A missing or null cursor means no bound. Sizes are clamped to 0..maxPage rather than refused, and
// a request that gives neither first nor last gets the first maxPage items.
func paginate(l *listing, a args) (*connection, error) {
	n := len(l.items)
	lo, hi := 0, n
	if a.has("after") {
		_, end, err := l.position(a.str("after"))
		if err != nil {
			return nil, err
		}
		lo = end
	}
	if a.has("before") {
		start, _, err := l.position(a.str("before"))
		if err != nil {
			return nil, err
		}
		hi = max(start, lo)
	}
	first, forward := a.int("first")
	last, backward := a.int("last")
	if !forward && !backward {
		first, forward = maxPage, true
	}
	if forward {
		hi = min(hi, lo+min(max(first, 0), maxPage))
	}
	if backward {
		lo = max(lo, hi-min(max(last, 0), maxPage))
	}
	return &connection{list: l, page: l.items[lo:hi], total: n, hasNext: hi < n, hasPrev: lo > 0}, nil
}

func (c *connection) edges() []any {
	out := make([]any, len(c.page))
	for i, item := range c.page {
		out[i] = &edge{cursor: cursorOf(c.list.key(item)), node: item}
	}
	return out
}

func (c *connection) info() *pageInfo {
	p := &pageInfo{hasNext: c.hasNext, hasPrev: c.hasPrev}
	if len(c.page) > 0 {
		p.start = cursorOf(c.list.key(c.page[0]))
		p.end = cursorOf(c.list.key(c.page[len(c.page)-1]))
	}
	return p
}

// connections registers the resolvers of the connection, edge and PageInfo types. Every
// connection type has the same shape, so they share their resolvers.
func connections(s *Schema) {
	for _, item := range []string{"Issue", "PullRequest", "PullRequestChangedFile", "IssueComment", "Label", "SearchResultItem"} {
		get(s, item+"Connection", getters[*connection]{
			"edges":    func(_ *query, c *connection) any { return c.edges() },
			"nodes":    func(_ *query, c *connection) any { return c.page },
			"pageInfo": func(_ *query, c *connection) any { return c.info() },
		})
		get(s, item+"Edge", getters[*edge]{
			"cursor": func(_ *query, e *edge) any { return e.cursor },
			"node":   func(_ *query, e *edge) any { return e.node },
		})
		if item == "SearchResultItem" {
			get(s, item+"Connection", getters[*connection]{
				"issueCount":      func(_ *query, c *connection) any { return c.counts[0] },
				"repositoryCount": func(_ *query, c *connection) any { return c.counts[1] },
				"userCount":       func(_ *query, c *connection) any { return c.counts[2] },
			})
		} else {
			get(s, item+"Connection", getters[*connection]{
				"totalCount": func(_ *query, c *connection) any { return c.total },
			})
		}
	}
	get(s, "PageInfo", getters[*pageInfo]{
		"hasNextPage":     func(_ *query, p *pageInfo) any { return p.hasNext },
		"hasPreviousPage": func(_ *query, p *pageInfo) any { return p.hasPrev },
		"startCursor":     func(_ *query, p *pageInfo) any { return p.start },
		"endCursor":       func(_ *query, p *pageInfo) any { return p.end },
	})
}
