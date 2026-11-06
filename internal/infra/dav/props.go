package dav

import (
	"encoding/xml"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
	"github.com/1995parham-me/farvardin/internal/infra/push"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

// syncToken is the bare tree id, exactly as Xandikos sends it, so a client
// keeps its token when the server changes between the two.
func syncToken(snap *store.Snapshot) string { return snap.Token }

// parseSyncToken accepts a bare id and the URI forms other servers use
// (anything ending in "/<id>").
func parseSyncToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}

	return s
}

func vline(name, value string) vcf.Line {
	return vcf.Line{Group: "", Name: name, Params: nil, Value: value, Spelling: ""}
}

func name(space, local string) xml.Name { return xml.Name{Space: space, Local: local} }

// allprop is what an empty PROPFIND body (or <allprop/>) returns.
var allprop = []xml.Name{
	name(NSDAV, "resourcetype"), name(NSDAV, "displayname"), name(NSDAV, "getetag"),
	name(NSDAV, "getcontenttype"), name(NSDAV, "getcontentlength"), name(NSDAV, "sync-token"),
	name(NSCS, "getctag"),
}

// requestedProps reads <prop>, <allprop> or <propname> from a PROPFIND or
// REPORT body. names is nil for allprop; propname is true for propname.
func requestedProps(body *node) ([]xml.Name, bool) {
	if body == nil || body.child(NSDAV, "allprop") != nil {
		return allprop, false
	}

	if body.child(NSDAV, "propname") != nil {
		return nil, true
	}

	prop := body.child(NSDAV, "prop")
	if prop == nil {
		return allprop, false
	}

	names := make([]xml.Name, 0, len(prop.Children))
	for _, c := range prop.Children {
		names = append(names, c.XMLName)
	}

	return names, false
}

func (h *Handler) servePropfind(w http.ResponseWriter, r *http.Request) {
	res, err := h.resolve(r.URL.Path)
	if err != nil {
		h.fail(w, r, err)

		return
	}

	if !res.exists {
		h.fail(w, r, errNotFound)

		return
	}

	body, err := readXML(r)
	if err != nil {
		h.fail(w, r, err)

		return
	}

	names, propname := requestedProps(body)
	depth := r.Header.Get("Depth")

	targets := []*resource{res}
	if depth == "1" || strings.EqualFold(depth, "infinity") {
		targets = append(targets, h.members(res)...)
	}

	responses := make([]*response, 0, len(targets))

	for _, t := range targets {
		resp := newResponse(h.resourceHref(t))

		if propname {
			for _, n := range allprop {
				resp.add(http.StatusOK, el(n.Space, n.Local))
			}
		} else {
			h.fillProps(resp, t, names, nil)
		}

		responses = append(responses, resp)
	}

	writeMultistatus(w, responses)
}

// members lists the children of a resource for Depth: 1.
func (h *Handler) members(res *resource) []*resource {
	var out []*resource

	base := resource{kind: resRoot, home: "", col: nil, name: "", obj: store.Object{}, snap: nil, exists: true}

	switch res.kind {
	case resRoot:
		p := base
		p.kind = resPrincipal
		out = append(out, &p)
	case resPrincipal:
		for _, k := range []store.Kind{store.AddressBook, store.Calendar} {
			hm := base
			hm.kind, hm.home = resHome, k
			out = append(out, &hm)
		}
	case resHome:
		for _, c := range h.collections(res.home) {
			snap, err := c.Snapshot()
			if err != nil {
				continue
			}

			cr := base
			cr.kind, cr.home, cr.col, cr.snap = resCollection, res.home, c, snap
			out = append(out, &cr)
		}
	case resCollection:
		h.metrics.Objects(string(res.home), res.col.Name(), len(res.snap.Objects))

		for _, o := range res.snap.Objects {
			or := base
			or.kind, or.home, or.col, or.snap, or.name, or.obj = resObject, res.home, res.col, res.snap, o.Name, o
			out = append(out, &or)
		}
	case resObject:
	}

	return out
}

// fillProps adds each requested property to resp, as 200 or 404.
// addressData carries the <address-data>/<calendar-data> request element
// of a REPORT, if any.
func (h *Handler) fillProps(resp *response, res *resource, names []xml.Name, dataReq *node) {
	for _, n := range names {
		if v := h.prop(res, n, dataReq); v != nil {
			resp.add(http.StatusOK, v)
		} else {
			resp.add(http.StatusNotFound, el(n.Space, n.Local))
		}
	}
}

func privileges(write bool) *elem {
	set := el(NSDAV, "current-user-privilege-set")
	names := []string{"read", "read-current-user-privilege-set"}

	if write {
		names = append(names, "write", "write-properties", "write-content", "bind", "unbind")
	}

	for _, n := range names {
		set.children = append(set.children, el(NSDAV, "privilege", el(NSDAV, n)))
	}

	return set
}

// prop returns one property of a resource, or nil when it has none.
//
//nolint:gocognit,gocyclo,cyclop,funlen,maintidx // a flat table of properties; splitting it would scatter it
func (h *Handler) prop(res *resource, n xml.Name, dataReq *node) *elem {
	isCol := res.kind != resObject
	_, canWrite := res.col.(writable)

	switch n {
	case name(NSDAV, "resourcetype"):
		rt := el(NSDAV, "resourcetype")

		switch res.kind {
		case resObject:
		case resPrincipal:
			rt.children = append(rt.children, el(NSDAV, "collection"), el(NSDAV, "principal"))
		case resCollection:
			rt.children = append(rt.children, el(NSDAV, "collection"))
			if res.home == store.AddressBook {
				rt.children = append(rt.children, el(NSCard, "addressbook"))
			} else {
				rt.children = append(rt.children, el(NSCal, "calendar"))
			}
		case resRoot, resHome:
			rt.children = append(rt.children, el(NSDAV, "collection"))
		}

		return rt
	case name(NSDAV, "displayname"):
		switch res.kind {
		case resCollection:
			return textEl(NSDAV, "displayname", displayName(res.col, res.snap))
		case resPrincipal:
			return textEl(NSDAV, "displayname", h.store.Principal())
		case resHome:
			return textEl(NSDAV, "displayname", res.home.Home())
		case resRoot, resObject:
		}
	case name(NSDAV, "current-user-principal"):
		return el(NSDAV, "current-user-principal", hrefEl(h.principalHref()))
	case name(NSDAV, "principal-URL"):
		if res.kind == resPrincipal {
			return el(NSDAV, "principal-URL", hrefEl(h.principalHref()))
		}
	case name(NSDAV, "principal-collection-set"):
		return el(NSDAV, "principal-collection-set", hrefEl(h.opts.Prefix+"/"))
	case name(NSDAV, "owner"):
		if res.kind == resCollection || res.kind == resObject {
			return el(NSDAV, "owner", hrefEl(h.principalHref()))
		}
	case name(NSDAV, "current-user-privilege-set"):
		if res.kind == resCollection || res.kind == resObject {
			return privileges(canWrite)
		}

		return privileges(res.kind == resHome)
	case name(NSDAV, "supported-report-set"):
		if !isCol {
			return nil
		}

		set := el(NSDAV, "supported-report-set")
		reports := []xml.Name{name(NSDAV, "sync-collection"), name(NSDAV, "expand-property")}

		switch res.home {
		case store.AddressBook:
			reports = append(reports, name(NSCard, "addressbook-multiget"), name(NSCard, "addressbook-query"))
		case store.Calendar:
			reports = append(reports, name(NSCal, "calendar-multiget"), name(NSCal, "calendar-query"))
		}

		for _, rn := range reports {
			set.children = append(set.children, el(NSDAV, "supported-report", el(NSDAV, "report", el(rn.Space, rn.Local))))
		}

		return set
	case name(NSDAV, "sync-token"):
		if res.kind == resCollection {
			return textEl(NSDAV, "sync-token", syncToken(res.snap))
		}
	case name(NSCS, "getctag"):
		if res.kind == resCollection {
			return textEl(NSCS, "getctag", syncToken(res.snap))
		}
	case name(NSDAV, "getetag"):
		if res.kind == resObject {
			return textEl(NSDAV, "getetag", `"`+res.obj.ETag+`"`)
		}
	case name(NSDAV, "getcontentlength"):
		if res.kind == resObject {
			return textEl(NSDAV, "getcontentlength", strconv.FormatInt(res.obj.Size, 10))
		}
	case name(NSDAV, "getcontenttype"):
		if res.kind == resObject {
			p := parsed{card: nil, cal: nil, err: nil}
			if res.home == store.Calendar {
				p = h.cache.get(res.col, res.obj.ETag)
			}

			return textEl(NSDAV, "getcontenttype", contentType(res.home, p))
		}
	case name(NSDAV, "resource-id"):
		if res.kind == resObject {
			return el(NSDAV, "resource-id", hrefEl("urn:farvardin:"+res.obj.ETag))
		}
	case name(NSCard, "addressbook-home-set"):
		return el(NSCard, "addressbook-home-set", hrefEl(h.homeHref(store.AddressBook)))
	case name(NSCal, "calendar-home-set"):
		return el(NSCal, "calendar-home-set", hrefEl(h.homeHref(store.Calendar)))
	case name(NSCal, "calendar-user-address-set"):
		set := el(NSCal, "calendar-user-address-set", hrefEl(h.principalHref()))
		if h.opts.CalendarUserAddress != "" {
			set.children = append([]*elem{hrefEl(h.opts.CalendarUserAddress)}, set.children...)
		}

		return set
	case name(NSCal, "calendar-user-type"):
		if res.kind == resPrincipal {
			return textEl(NSCal, "calendar-user-type", "INDIVIDUAL")
		}
	case name(NSCard, "addressbook-description"):
		if res.kind == resCollection && res.home == store.AddressBook {
			return textEl(NSCard, "addressbook-description", res.snap.Meta().Description())
		}
	case name(NSCal, "calendar-description"):
		if res.kind == resCollection && res.home == store.Calendar {
			return textEl(NSCal, "calendar-description", res.snap.Meta().Description())
		}
	case name(NSApple, "calendar-color"):
		if res.kind == resCollection && res.snap.Meta().Color() != "" {
			return textEl(NSApple, "calendar-color", res.snap.Meta().Color())
		}
	case name(NSApple, "calendar-order"):
		if res.kind == resCollection && res.home == store.Calendar {
			return textEl(NSApple, "calendar-order", strconv.Itoa(res.snap.Meta().Order()))
		}
	case name(NSCard, "supported-address-data"):
		if res.kind == resCollection && res.home == store.AddressBook {
			return el(NSCard, "supported-address-data",
				el(NSCard, "address-data-type").withAttr("content-type", "text/vcard").withAttr("version", "3.0"),
				el(NSCard, "address-data-type").withAttr("content-type", "text/vcard").withAttr("version", "4.0"),
				el(NSCard, "address-data-type").withAttr("content-type", "application/jscontact+json").withAttr("version", "1.0"),
			)
		}
	case name(NSCal, "supported-calendar-data"):
		if res.kind == resCollection && res.home == store.Calendar {
			return el(NSCal, "supported-calendar-data", el(NSCal, "calendar-data").withAttr("content-type", "text/calendar").withAttr("version", "2.0"))
		}
	case name(NSCal, "supported-calendar-component-set"):
		if res.kind == resCollection && res.home == store.Calendar {
			comps := supportedComponents(res.snap.Meta())
			if len(comps) == 0 {
				comps = []string{"VEVENT", "VTODO", "VJOURNAL"}
			}

			set := el(NSCal, "supported-calendar-component-set")
			for _, c := range comps {
				set.children = append(set.children, el(NSCal, "comp").withAttr("name", strings.ToUpper(strings.TrimSpace(c))))
			}

			return set
		}
	case name(NSCard, "max-resource-size"), name(NSCal, "max-resource-size"):
		if res.kind == resCollection {
			return textEl(n.Space, n.Local, strconv.FormatInt(h.opts.MaxResourceSize, 10))
		}
	case name(NSCard, "address-data"), name(NSCal, "calendar-data"):
		if res.kind == resObject {
			return h.objectData(res, n, dataReq)
		}
	case name(push.Namespace, "transports"), name(push.Namespace, "topic"), name(push.Namespace, "supported-triggers"):
		return h.pushProp(res, n)
	}

	return nil
}

// objectData returns <address-data> or <calendar-data> for an object,
// converted to the vCard version the request asked for.
func (h *Handler) objectData(res *resource, n xml.Name, dataReq *node) *elem {
	data, err := res.col.ReadBlob(res.obj.ETag)
	if err != nil {
		return nil
	}

	if n.Space == NSCard && dataReq != nil {
		want := dataReq.attr("version")
		if strings.Contains(dataReq.attr("content-type"), "jscontact") {
			want = "jscontact"
		}

		if want != "" {
			if out, _, err := convertCard(data, want); err == nil {
				data = out
			}
		}
	}

	return textEl(n.Space, n.Local, string(data))
}

// errPropNotWritable marks a property PROPPATCH cannot set.
var errPropNotWritable = errors.New("property is protected or unknown")

// applyProp sets (or removes) one writable collection property in meta.
func applyProp(meta *store.Meta, p *node, remove bool) error {
	value := strings.TrimSpace(p.Text)
	if remove {
		value = ""
	}

	switch p.XMLName {
	case name(NSDAV, "displayname"):
		meta.Set("", "displayname", value)
	case name(NSCard, "addressbook-description"), name(NSCal, "calendar-description"):
		meta.Set("", "description", value)
	case name(NSApple, "calendar-color"):
		meta.Set("", "color", value)
	case name(NSApple, "calendar-order"):
		meta.Set("calendar", "order", value)
	case name(NSCal, "supported-calendar-component-set"):
		found := p.children(NSCal, "comp")
		comps := make([]string, 0, len(found))

		for _, c := range found {
			comps = append(comps, c.attr("name"))
		}

		meta.Set("", "components", strings.Join(comps, ","))
	case name(NSDAV, "resourcetype"):
		// Sent by extended MKCOL; the kind comes from the path.
	default:
		return errPropNotWritable
	}

	return nil
}

func (h *Handler) serveProppatch(w http.ResponseWriter, r *http.Request) {
	res, err := h.resolve(r.URL.Path)
	if err != nil {
		h.fail(w, r, err)

		return
	}

	body, err := readXML(r)
	if err != nil || body == nil {
		http.Error(w, "PROPPATCH needs a propertyupdate body", http.StatusBadRequest)

		return
	}

	col, ok := res.col.(writable)
	if res.kind != resCollection || !ok {
		writeError(w, http.StatusForbidden, el(NSDAV, "cannot-modify-protected-property"), "only collection properties can be changed")

		return
	}

	meta := res.snap.Meta()
	resp := newResponse(h.resourceHref(res))

	var (
		applied []xml.Name
		failed  bool
	)

	for _, op := range body.Children {
		remove := op.is(NSDAV, "remove")
		if !remove && !op.is(NSDAV, "set") {
			continue
		}

		prop := op.child(NSDAV, "prop")
		if prop == nil {
			continue
		}

		for _, p := range prop.Children {
			if err := applyProp(&meta, &p, remove); err != nil {
				failed = true

				resp.add(http.StatusForbidden, el(p.XMLName.Space, p.XMLName.Local))

				continue
			}

			applied = append(applied, p.XMLName)
		}
	}

	// PROPPATCH is atomic: if one property failed, none is stored.
	status := http.StatusOK
	if failed {
		status = http.StatusFailedDependency
	} else if err := col.SetMeta(meta, requester(r)); err != nil {
		h.fail(w, r, err)

		return
	} else {
		names := make([]string, 0, len(applied))
		for _, n := range applied {
			names = append(names, "{"+n.Space+"}"+n.Local)
		}

		h.notifyProperties(r, res.col, names)
	}

	for _, n := range applied {
		resp.add(status, el(n.Space, n.Local))
	}

	writeMultistatus(w, []*response{resp})
}
