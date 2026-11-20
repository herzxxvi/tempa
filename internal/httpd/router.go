package httpd

import "strings"

// maxParams is the most parameters one route pattern may capture.
const maxParams = 8

// Router matches request paths against patterns, segment by segment, without allocating.
//
// A pattern is a path made of literal segments, single-segment parameters "{name}" and, as the last
// segment only, a catch-all "{name...}" that captures the rest of the path including slashes:
//
//	/repos/{owner}/{repo}/issues/{number}
//	/repos/{owner}/{repo}/contents/{path...}
//
// Literal segments win over parameters, parameters over a catch-all, and the search backtracks, so
// "/git/refs/heads/{branch...}" and "/git/refs/{ref...}" can coexist. A trailing slash is ignored.
// A catch-all needs at least one character: "/contents" and "/contents/" match the pattern
// "/contents", never "/contents/{path...}".
// Captured values are raw (not percent-decoded) views into the request; read them with Ctx.Param in
// pattern order. Parameter names are documentation only.
type Router struct {
	root node
}

type node struct {
	literals []literal
	param    *node
	rest     *node // catch-all; only its handlers are used
	handlers [numMethods]Handler
}

type literal struct {
	segment string
	next    *node
}

// Handle registers h for method and pattern. It panics on a malformed pattern or a duplicate
// registration: both are programming errors that must show at start-up.
func (r *Router) Handle(method Method, pattern string, h Handler) {
	if !strings.HasPrefix(pattern, "/") {
		panic("httpd: route pattern must start with '/': " + pattern)
	}
	n := &r.root
	params := 0
	segments := strings.Split(strings.Trim(pattern, "/"), "/")
	if len(segments) == 1 && segments[0] == "" {
		segments = nil
	}
	for i, seg := range segments {
		switch {
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "...}"):
			if i != len(segments)-1 {
				panic("httpd: a catch-all must be the last segment: " + pattern)
			}
			if n.rest == nil {
				n.rest = &node{}
			}
			n = n.rest
			params++
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"):
			if n.param == nil {
				n.param = &node{}
			}
			n = n.param
			params++
		case seg == "":
			panic("httpd: empty segment in route pattern: " + pattern)
		default:
			next := (*node)(nil)
			for j := range n.literals {
				if n.literals[j].segment == seg {
					next = n.literals[j].next
				}
			}
			if next == nil {
				next = &node{}
				n.literals = append(n.literals, literal{seg, next})
			}
			n = next
		}
	}
	if params > maxParams {
		panic("httpd: too many parameters in route pattern: " + pattern)
	}
	if n.handlers[method] != nil {
		panic("httpd: route registered twice: " + pattern)
	}
	n.handlers[method] = h
}

// Find returns the handler for (method, path) and leaves the captured parameters in c. It returns
// nil when no pattern matches the path or the matching pattern has no handler for the method.
func (r *Router) Find(c *Ctx, method Method, path []byte) Handler {
	c.nparams = 0
	if method >= numMethods {
		return nil
	}
	return r.root.find(c, method, path)
}

func (n *node) find(c *Ctx, method Method, path []byte) Handler {
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	if len(path) == 0 {
		return n.handlers[method]
	}
	seg, tail := path, []byte(nil)
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			seg, tail = path[:i], path[i:]
			break
		}
	}
	for i := range n.literals {
		if string(seg) == n.literals[i].segment {
			if h := n.literals[i].next.find(c, method, tail); h != nil {
				return h
			}
			break
		}
	}
	saved := c.nparams
	if n.param != nil && len(seg) > 0 && saved < maxParams {
		c.params[saved] = seg
		c.nparams = saved + 1
		if h := n.param.find(c, method, tail); h != nil {
			return h
		}
		c.nparams = saved
	}
	if n.rest != nil && saved < maxParams {
		if h := n.rest.handlers[method]; h != nil {
			c.params[saved] = path
			c.nparams = saved + 1
			return h
		}
	}
	return nil
}

// Param returns the i-th captured parameter of the matched route (0-based, in pattern order), raw.
func (c *Ctx) Param(i int) []byte {
	if i < 0 || i >= c.nparams {
		return nil
	}
	return c.params[i]
}

// ParamString returns the i-th parameter percent-decoded. ok is false when it is malformed.
func (c *Ctx) ParamString(i int) (s string, ok bool) {
	raw := c.Param(i)
	if !NeedsUnescape(raw) {
		return string(raw), true
	}
	var buf [128]byte
	dec, ok := Unescape(buf[:0], raw, false)
	return string(dec), ok
}

// ParamInt returns the i-th parameter as a decimal number. ok is false for anything but digits or
// a value beyond 18 digits.
func (c *Ctx) ParamInt(i int) (int64, bool) {
	return ParseInt(c.Param(i))
}
