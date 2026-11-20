package httpd

import (
	"bytes"
	"strconv"
)

// Every response is assembled completely in the connection's output buffer and leaves in one write.
// The header set is minimal on purpose: each extra header is bytes on the wire and parsing work for
// the client, and nothing reads Date, Server or cache headers.
//
// JSON replies look like this (the request id is a process-wide counter, never empty):
//
//	HTTP/1.1 200 OK
//	Content-Type: application/json; charset=utf-8
//	X-GitHub-Request-Id: 000000000000002A
//	Content-Length: 517
//
//	{...}

const jsonType = "Content-Type: application/json; charset=utf-8\r\nX-GitHub-Request-Id: "

// statusLines[status] is the status line and jsonHeads[status] is that line plus the fixed JSON
// headers up to where the request id goes. Both are built once for the statuses in use.
var (
	statusLines [600]string
	jsonHeads   [600][]byte
)

func init() {
	for _, status := range []int{200, 201, 202, 204, 304, 400, 401, 403, 404, 405, 409, 410, 413, 415, 422, 431, 500, 501, 505} {
		statusLines[status] = "HTTP/1.1 " + strconv.Itoa(status) + " " + statusText(status) + "\r\n"
		jsonHeads[status] = []byte(statusLines[status] + jsonType)
	}
}

func statusLine(status int) string {
	if status > 0 && status < len(statusLines) && statusLines[status] != "" {
		return statusLines[status]
	}
	return "HTTP/1.1 " + strconv.Itoa(status) + " " + statusText(status) + "\r\n"
}

func statusText(status int) string {
	switch status {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 304:
		return "Not Modified"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 410:
		return "Gone"
	case 413:
		return "Content Too Large"
	case 415:
		return "Unsupported Media Type"
	case 422:
		return "Unprocessable Entity"
	case 431:
		return "Request Header Fields Too Large"
	case 500:
		return "Internal Server Error"
	case 501:
		return "Not Implemented"
	case 505:
		return "HTTP Version Not Supported"
	}
	return "Status"
}

// errorBody is the JSON body the engine itself sends for a status it produced.
func errorBody(status int) []byte {
	return []byte(`{"message":"` + statusText(status) + `"}`)
}

// begin starts a reply. A second reply to the same request is a handler bug; the panic turns into
// a 500 and a stack trace on stderr.
func (c *Ctx) begin() {
	if c.replied {
		panic("httpd: the handler replied twice to one request")
	}
	c.replied = true
}

// appendJSONHead appends the status line, the JSON content type and the request id header.
func (c *Ctx) appendJSONHead(status int) {
	if status > 0 && status < len(jsonHeads) && jsonHeads[status] != nil {
		c.out = append(c.out, jsonHeads[status]...)
	} else {
		c.out = append(c.out, statusLine(status)...)
		c.out = append(c.out, jsonType...)
	}
	c.appendRequestID()
}

// appendRequestID appends 16 hex digits of the request counter and the line end.
func (c *Ctx) appendRequestID() {
	v := c.srv.seq.Add(1)
	n := len(c.out)
	c.out = append(c.out, "0000000000000000\r\n"...)
	for i := n + 15; v != 0; i-- {
		c.out[i] = "0123456789ABCDEF"[v&15]
		v >>= 4
	}
}

// appendFraming appends the per-reply extra headers and, when the connection is about to be closed,
// the header that says so.
func (c *Ctx) appendFraming() {
	c.out = append(c.out, c.extra...)
	if c.closeAfter {
		c.out = append(c.out, "Connection: close\r\n"...)
	}
}

func (c *Ctx) appendBody(body []byte) {
	c.out = append(c.out, "Content-Length: "...)
	c.out = strconv.AppendInt(c.out, int64(len(body)), 10)
	c.out = append(c.out, "\r\n\r\n"...)
	if !c.isHead {
		c.out = append(c.out, body...)
	}
}

// JSON replies with a JSON body: Content-Type application/json; charset=utf-8, a request id and the
// headers added with AddHeader. body is copied; it may be a cached slice.
func (c *Ctx) JSON(status int, body []byte) {
	c.begin()
	c.appendJSONHead(status)
	c.appendFraming()
	c.appendBody(body)
}

// AppendTail appends the pre-rendered tail of a JSON reply to dst: the Content-Length header, the
// blank line and body. Objects that are read far more often than they change keep their tail, and
// JSONTail then answers with two appends.
func AppendTail(dst, body []byte) []byte {
	dst = append(dst, "Content-Length: "...)
	dst = strconv.AppendInt(dst, int64(len(body)), 10)
	dst = append(dst, "\r\n\r\n"...)
	return append(dst, body...)
}

// TailBody returns the body inside a tail built by AppendTail.
func TailBody(tail []byte) []byte {
	return tail[bytes.Index(tail, crlfcrlf)+4:]
}

// JSONTail is JSON for a tail built by AppendTail.
func (c *Ctx) JSONTail(status int, tail []byte) {
	c.begin()
	c.appendJSONHead(status)
	c.appendFraming()
	if c.isHead {
		tail = tail[:bytes.Index(tail, crlfcrlf)+4]
	}
	c.out = append(c.out, tail...)
}

// Bytes replies with a body of any content type and no request id header (the git surface).
func (c *Ctx) Bytes(status int, contentType string, body []byte) {
	c.begin()
	c.out = append(c.out, statusLine(status)...)
	c.out = append(c.out, "Content-Type: "...)
	c.out = append(c.out, contentType...)
	c.out = append(c.out, "\r\n"...)
	c.appendFraming()
	c.appendBody(body)
}

// Prebuilt replies with a complete, pre-rendered response: status line, headers (including
// Content-Length), blank line and body. Extra headers do not apply.
func (c *Ctx) Prebuilt(response []byte) {
	c.begin()
	c.out = append(c.out, response...)
}

// AppendResponse appends a complete response with the given content type to dst, for callers that
// cache whole responses and later send them with Prebuilt.
func AppendResponse(dst []byte, status int, contentType string, body []byte) []byte {
	dst = append(dst, statusLine(status)...)
	dst = append(dst, "Content-Type: "...)
	dst = append(dst, contentType...)
	dst = append(dst, "\r\nContent-Length: "...)
	dst = strconv.AppendInt(dst, int64(len(body)), 10)
	dst = append(dst, "\r\n\r\n"...)
	return append(dst, body...)
}

// Empty replies without a body. 204 and 304 carry no Content-Length; any other status says 0.
func (c *Ctx) Empty(status int) {
	c.begin()
	c.out = append(c.out, statusLine(status)...)
	c.out = append(c.out, "X-GitHub-Request-Id: "...)
	c.appendRequestID()
	c.appendFraming()
	if status != 204 && status != 304 {
		c.out = append(c.out, "Content-Length: 0\r\n"...)
	}
	c.out = append(c.out, "\r\n"...)
}

// AddHeader adds a header to the reply that follows. Line breaks in value are replaced by spaces.
func (c *Ctx) AddHeader(name, value string) {
	c.extra = append(c.extra, name...)
	c.extra = append(c.extra, ": "...)
	n := len(c.extra)
	c.extra = append(c.extra, value...)
	c.endHeader(n)
}

// AddHeaderBytes is AddHeader for a value built in a byte slice.
func (c *Ctx) AddHeaderBytes(name string, value []byte) {
	c.extra = append(c.extra, name...)
	c.extra = append(c.extra, ": "...)
	n := len(c.extra)
	c.extra = append(c.extra, value...)
	c.endHeader(n)
}

func (c *Ctx) endHeader(valueStart int) {
	for i := valueStart; i < len(c.extra); i++ {
		if c.extra[i] == '\r' || c.extra[i] == '\n' {
			c.extra[i] = ' '
		}
	}
	c.extra = append(c.extra, "\r\n"...)
}

// Close asks the engine to close the connection after the reply to this request has been written.
func (c *Ctx) Close() { c.closeAfter = true }
