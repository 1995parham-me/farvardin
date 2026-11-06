package dav

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/calendar"
	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

func (h *Handler) serveReport(w http.ResponseWriter, r *http.Request) {
	res, err := h.resolve(r.URL.Path)
	if err != nil {
		h.fail(w, r, err)

		return
	}

	body, err := readXML(r)
	if err != nil || body == nil {
		http.Error(w, "REPORT needs a body", http.StatusBadRequest)

		return
	}

	switch {
	case body.is(NSDAV, "sync-collection"):
		h.reportSync(w, r, res, body)
	case body.is(NSCard, "addressbook-multiget"), body.is(NSCal, "calendar-multiget"):
		h.reportMultiget(w, body)
	case body.is(NSCard, "addressbook-query"):
		h.reportQuery(w, res, body, h.matchCard)
	case body.is(NSCal, "calendar-query"):
		h.reportQuery(w, res, body, h.matchEvent)
	case body.is(NSDAV, "expand-property"), body.is(NSDAV, "principal-property-search"),
		body.is(NSDAV, "principal-search-property-set"):
		// Single-principal server: there is nothing to search or expand.
		writeMultistatus(w, nil)
	default:
		writeError(w, http.StatusForbidden, el(NSDAV, "supported-report"), "report "+body.XMLName.Local+" is not supported")
	}
}

// dataRequest returns the <address-data>/<calendar-data> element inside
// the requested <prop>, which may carry a version or content-type.
func dataRequest(body *node) *node {
	prop := body.child(NSDAV, "prop")
	if prop == nil {
		return nil
	}

	if d := prop.child(NSCard, "address-data"); d != nil {
		return d
	}

	return prop.child(NSCal, "calendar-data")
}

func (h *Handler) objectResource(col *resource, o store.Object) *resource {
	return &resource{kind: resObject, home: col.home, col: col.col, name: o.Name, obj: o, snap: col.snap, exists: true}
}

func (h *Handler) reportSync(w http.ResponseWriter, r *http.Request, res *resource, body *node) {
	if res.kind != resCollection {
		writeError(w, http.StatusForbidden, el(NSDAV, "supported-report"), "sync-collection works on collections")

		return
	}

	token := ""
	if t := body.child(NSDAV, "sync-token"); t != nil {
		token = parseSyncToken(t.Text)
	}

	changed, deleted, snap, err := changes(res.col, token)
	if errors.Is(err, store.ErrInvalidToken) {
		writeError(w, http.StatusForbidden, el(NSDAV, "valid-sync-token"), "unknown sync token; sync again from scratch")

		return
	}

	if err != nil {
		h.fail(w, r, err)

		return
	}

	res.snap = snap
	names, _ := requestedProps(body)
	dataReq := dataRequest(body)
	ms := startMultistatus(w)

	for _, o := range changed {
		obj := h.objectResource(res, o)
		resp := newResponse(h.resourceHref(obj))
		h.fillProps(resp, obj, names, dataReq)
		ms.add(resp)
	}

	for _, n := range deleted {
		resp := newResponse(h.objectHref(res.home, res.col.Name(), n))
		resp.status = http.StatusNotFound
		ms.add(resp)
	}

	ms.close(textEl(NSDAV, "sync-token", syncToken(snap)))
}

func (h *Handler) reportMultiget(w http.ResponseWriter, body *node) {
	names, _ := requestedProps(body)
	dataReq := dataRequest(body)
	hrefs := body.children(NSDAV, "href")

	if len(hrefs) > h.opts.MaxMultigetHrefs {
		writeError(w, http.StatusForbidden, el(NSDAV, "number-of-matches-within-limits"),
			fmt.Sprintf("ask for at most %d objects per multiget", h.opts.MaxMultigetHrefs))

		return
	}

	ms := startMultistatus(w)
	defer ms.close()

	for _, hn := range hrefs {
		href := strings.TrimSpace(hn.Text)

		p := href
		if u, err := url.Parse(href); err == nil {
			p = u.Path
		}

		obj, err := h.resolve(p)
		if err != nil || obj.kind != resObject || !obj.exists {
			resp := newResponse(href)
			resp.status = http.StatusNotFound
			ms.add(resp)

			continue
		}

		resp := newResponse(h.resourceHref(obj))
		h.fillProps(resp, obj, names, dataReq)
		ms.add(resp)
	}
}

type matcher func(col collection, o store.Object, filter *node) bool

func (h *Handler) reportQuery(w http.ResponseWriter, res *resource, body *node, match matcher) {
	if res.kind != resCollection {
		writeError(w, http.StatusForbidden, el(NSDAV, "supported-report"), "queries run on collections")

		return
	}

	names, _ := requestedProps(body)
	dataReq := dataRequest(body)
	filter := body.child(NSCard, "filter")

	if filter == nil {
		filter = body.child(NSCal, "filter")
	}

	limit := 0

	if l := body.child(NSCard, "limit"); l != nil {
		if n := l.child(NSCard, "nresults"); n != nil {
			limit, _ = strconv.Atoi(strings.TrimSpace(n.Text))
		}
	}

	ms := startMultistatus(w)
	defer ms.close()

	n := 0

	for _, o := range res.snap.Objects {
		if filter != nil && !match(res.col, o, filter) {
			continue
		}

		obj := h.objectResource(res, o)
		resp := newResponse(h.resourceHref(obj))
		h.fillProps(resp, obj, names, dataReq)
		ms.add(resp)

		if n++; limit > 0 && n >= limit {
			break
		}
	}
}

// matchCard evaluates an RFC 6352 section 10.5 filter.
func (h *Handler) matchCard(col collection, o store.Object, filter *node) bool {
	p := h.cache.get(col, o.ETag)
	if p.card == nil {
		return false
	}

	propFilters := filter.children(NSCard, "prop-filter")
	if len(propFilters) == 0 {
		return true
	}

	anyOf := !strings.EqualFold(filter.attr("test"), "allof")

	for _, pf := range propFilters {
		ok := matchPropFilter(*p.card, pf)
		if ok && anyOf {
			return true
		}

		if !ok && !anyOf {
			return false
		}
	}

	return !anyOf
}

func matchPropFilter(c contact.Card, pf *node) bool {
	lines := c.All(strings.ToUpper(pf.attr("name")))

	if pf.child(NSCard, "is-not-defined") != nil {
		return len(lines) == 0
	}

	tests := pf.children(NSCard, "text-match")
	if len(tests) == 0 {
		return len(lines) > 0
	}

	allOf := strings.EqualFold(pf.attr("test"), "allof")

	for _, tm := range tests {
		hit := false

		for _, l := range lines {
			if textMatch(l.Text(), tm) {
				hit = true

				break
			}
		}

		if hit && !allOf {
			return true
		}

		if !hit && allOf {
			return false
		}
	}

	return allOf
}

// textMatch implements the i;unicode-casemap and i;ascii-casemap
// collations with the four match types.
func textMatch(value string, tm *node) bool {
	needle := strings.TrimSpace(tm.Text)

	if !strings.EqualFold(tm.attr("collation"), "i;octet") {
		value, needle = strings.ToLower(value), strings.ToLower(needle)
	}

	var ok bool

	switch strings.ToLower(tm.attr("match-type")) {
	case "equals":
		ok = value == needle
	case "starts-with":
		ok = strings.HasPrefix(value, needle)
	case "ends-with":
		ok = strings.HasSuffix(value, needle)
	default:
		ok = strings.Contains(value, needle)
	}

	if strings.EqualFold(tm.attr("negate-condition"), "yes") {
		return !ok
	}

	return ok
}

// matchEvent evaluates an RFC 4791 section 9.7 filter. Component names
// and time ranges are checked; property filters are treated as matching,
// which returns a superset the client filters again.
func (h *Handler) matchEvent(col collection, o store.Object, filter *node) bool {
	p := h.cache.get(col, o.ETag)
	if p.cal == nil {
		return false
	}

	top := filter.child(NSCal, "comp-filter")
	if top == nil || !strings.EqualFold(top.attr("name"), "VCALENDAR") {
		return true
	}

	for _, cf := range top.children(NSCal, "comp-filter") {
		if !matchCompFilter(p.cal, cf) {
			return false
		}
	}

	return true
}

func matchCompFilter(cal *calendar.Component, cf *node) bool {
	compName := strings.ToUpper(cf.attr("name"))

	var comps []*calendar.Component

	for _, c := range cal.Components {
		if c.Name == compName {
			comps = append(comps, c)
		}
	}

	if cf.child(NSCal, "is-not-defined") != nil {
		return len(comps) == 0
	}

	if len(comps) == 0 {
		return false
	}

	if tr := cf.child(NSCal, "time-range"); tr != nil {
		start, _ := parseUTC(tr.attr("start"))
		end, _ := parseUTC(tr.attr("end"))

		only := &calendar.Component{Name: "VCALENDAR", Props: nil, Components: comps}

		return calendar.Overlaps(only, start, end)
	}

	return true
}

func parseUTC(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}

	t, err := time.Parse("20060102T150405Z", s)
	if err != nil {
		return time.Time{}, err //nolint:wrapcheck // an unreadable bound means an open range
	}

	return t, nil
}
