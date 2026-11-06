package dav

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// XML namespaces.
const (
	NSDAV   = "DAV:"
	NSCard  = "urn:ietf:params:xml:ns:carddav"
	NSCal   = "urn:ietf:params:xml:ns:caldav"
	NSCS    = "http://calendarserver.org/ns/"
	NSApple = "http://apple.com/ns/ical/"
	NSFV    = "https://github.com/1995parham-me/farvardin/ns"
)

var prefixes = map[string]string{
	NSDAV: "d", NSCard: "card", NSCal: "cal", NSCS: "cs", NSApple: "ical", NSFV: "fv",
}

// errBadXML is returned for a request body that is not well-formed.
var errBadXML = errors.New("malformed XML request body")

// node is a generic parsed XML element.
type node struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Children []node     `xml:",any"`
	Text     string     `xml:",chardata"`
}

func (n *node) is(space, local string) bool {
	return n.XMLName.Space == space && n.XMLName.Local == local
}

func (n *node) child(space, local string) *node {
	for i := range n.Children {
		if n.Children[i].is(space, local) {
			return &n.Children[i]
		}
	}

	return nil
}

func (n *node) children(space, local string) []*node {
	var out []*node

	for i := range n.Children {
		if n.Children[i].is(space, local) {
			out = append(out, &n.Children[i])
		}
	}

	return out
}

func (n *node) attr(local string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}

	return ""
}

// maxXMLBody bounds PROPFIND/REPORT/PROPPATCH bodies.
const maxXMLBody = 1 << 20

// readXML parses a request body; an empty body returns nil.
func readXML(r *http.Request) (*node, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxXMLBody))
	if err != nil {
		return nil, fmt.Errorf("reading request body: %w", err)
	}

	if strings.TrimSpace(string(body)) == "" {
		return nil, nil //nolint:nilnil // an empty body is allowed and means "defaults"
	}

	var n node
	if err := xml.Unmarshal(body, &n); err != nil { //nolint:musttag // node's XMLName needs no tag; its other fields are tagged
		return nil, fmt.Errorf("%w: %w", errBadXML, err)
	}

	return &n, nil
}

// elem is an XML element in a response.
type elem struct {
	name     xml.Name
	attrs    []xml.Attr
	text     string
	children []*elem
}

func el(space, local string, children ...*elem) *elem {
	return &elem{name: xml.Name{Space: space, Local: local}, attrs: nil, text: "", children: children}
}

func textEl(space, local, text string) *elem {
	e := el(space, local)
	e.text = text

	return e
}

func hrefEl(href string) *elem { return textEl(NSDAV, "href", href) }

func (e *elem) withAttr(name, value string) *elem {
	e.attrs = append(e.attrs, xml.Attr{Name: xml.Name{Space: "", Local: name}, Value: value})

	return e
}

func (e *elem) write(w *bufio.Writer, root bool) {
	prefix, known := prefixes[e.name.Space]

	w.WriteByte('<')

	if e.name.Space != "" {
		if !known {
			prefix = "x"
		}

		w.WriteString(prefix + ":")
	}

	w.WriteString(e.name.Local)

	if root {
		keys := make([]string, 0, len(prefixes))
		for ns := range prefixes {
			keys = append(keys, ns)
		}

		sort.Strings(keys)

		for _, ns := range keys {
			fmt.Fprintf(w, ` xmlns:%s="%s"`, prefixes[ns], ns)
		}
	}

	if e.name.Space != "" && !known {
		w.WriteString(` xmlns:x="`)
		escapeText(w, e.name.Space, true)
		w.WriteByte('"')
	}

	for _, a := range e.attrs {
		w.WriteString(" " + a.Name.Local + `="`)
		escapeText(w, a.Value, true)
		w.WriteByte('"')
	}

	if e.text == "" && len(e.children) == 0 {
		w.WriteString("/>")

		return
	}

	w.WriteByte('>')
	escapeText(w, e.text, false)

	for _, c := range e.children {
		c.write(w, false)
	}

	w.WriteString("</")

	if e.name.Space != "" {
		w.WriteString(prefix + ":")
	}

	w.WriteString(e.name.Local + ">")
}

// escapeText escapes the XML special characters. CR is escaped too, so
// the CRLF line endings of vCard and iCalendar data survive XML parsing
// (a parser turns a literal CRLF into LF).
func escapeText(w *bufio.Writer, s string, attr bool) {
	last := 0

	for i := range len(s) {
		var esc string

		switch s[i] {
		case '&':
			esc = "&amp;"
		case '<':
			esc = "&lt;"
		case '>':
			esc = "&gt;"
		case '\r':
			esc = "&#13;"
		case '"':
			if !attr {
				continue
			}

			esc = "&quot;"
		default:
			continue
		}

		w.WriteString(s[last:i])
		w.WriteString(esc)
		last = i + 1
	}

	w.WriteString(s[last:])
}

// writeXML sends one XML document.
func writeXML(w http.ResponseWriter, status int, root *elem) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(status)

	bw := bufio.NewWriterSize(w, 64*1024)
	bw.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	root.write(bw, true)
	_ = bw.Flush()
}

func statusLine(code int) string {
	return "HTTP/1.1 " + strconv.Itoa(code) + " " + http.StatusText(code)
}

// response is one <d:response> of a multistatus.
type response struct {
	href string
	// status, when non-zero, is the status of the whole resource (for
	// example 404 for a deleted object in a sync report).
	status    int
	propstats map[int][]*elem
}

func newResponse(href string) *response {
	return &response{href: href, status: 0, propstats: map[int][]*elem{}}
}

func (r *response) add(status int, e *elem) {
	r.propstats[status] = append(r.propstats[status], e)
}

func (r *response) elem() *elem {
	e := el(NSDAV, "response", hrefEl(r.href))

	if r.status != 0 {
		e.children = append(e.children, textEl(NSDAV, "status", statusLine(r.status)))

		return e
	}

	codes := make([]int, 0, len(r.propstats))
	for c := range r.propstats {
		codes = append(codes, c)
	}

	sort.Ints(codes)

	for _, code := range codes {
		e.children = append(e.children, el(NSDAV, "propstat",
			el(NSDAV, "prop", r.propstats[code]...),
			textEl(NSDAV, "status", statusLine(code)),
		))
	}

	return e
}

// writeMultistatus streams a 207 response. extra elements (such as the
// sync-token of a sync report) follow the responses.
func writeMultistatus(w http.ResponseWriter, responses []*response, extra ...*elem) {
	root := el(NSDAV, "multistatus")

	for _, r := range responses {
		root.children = append(root.children, r.elem())
	}

	root.children = append(root.children, extra...)

	writeXML(w, http.StatusMultiStatus, root)
}

// writeError sends a DAV:error body naming the failed precondition.
func writeError(w http.ResponseWriter, status int, precondition *elem, message string) {
	root := el(NSDAV, "error")
	if precondition != nil {
		root.children = append(root.children, precondition)
	}

	if message != "" {
		root.children = append(root.children, textEl(NSFV, "message", message))
	}

	writeXML(w, status, root)
}
