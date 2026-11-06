// Package dav serves the store over WebDAV (RFC 4918), CardDAV
// (RFC 6352), CalDAV (RFC 4791) and WebDAV sync (RFC 6578).
package dav

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/1995parham-me/farvardin/internal/domain/calendar"
	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/metrics"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

// Options configures the handler.
type Options struct {
	// Prefix is the URL path the DAV tree is mounted at, "" for the root.
	Prefix string
	// UIPath is where browsers are sent when they open a collection.
	UIPath string
	// MaxResourceSize is the largest object a client may PUT, in bytes.
	MaxResourceSize int64
	// Photo is applied to every card a client writes.
	Photo contact.PhotoPolicy
	// Birthdays adds the generated birthdays calendar.
	Birthdays bool
	// BirthdayAlarmDays adds a reminder to each birthday; 0 for none.
	BirthdayAlarmDays int
	// BirthdayColor is the color of the birthdays calendar.
	BirthdayColor string
	// AllowCollectionDelete lets clients DELETE a whole collection.
	AllowCollectionDelete bool
	// CalendarUserAddress is published as the principal's
	// calendar-user-address-set, for example "mailto:me@example.com".
	CalendarUserAddress string
}

// Handler is the DAV http.Handler.
type Handler struct {
	store   *store.Store
	opts    Options
	log     *slog.Logger
	metrics *metrics.Metrics
	cache   *cache

	bdayMu sync.Mutex
	bday   *birthdays
}

// New returns a handler for s.
func New(s *store.Store, opts Options, log *slog.Logger, m *metrics.Metrics) *Handler {
	opts.Prefix = strings.TrimSuffix(opts.Prefix, "/")
	if opts.MaxResourceSize <= 0 {
		opts.MaxResourceSize = 10 << 20
	}

	return &Handler{store: s, opts: opts, log: log, metrics: m, cache: newCache(20000), bdayMu: sync.Mutex{}, bday: nil}
}

// Cache exposes parsed objects to the web UI so both share one cache.
func (h *Handler) Cache() func(col *store.Collection, etag string) (*contact.Card, *calendar.Component, error) {
	return func(col *store.Collection, etag string) (*contact.Card, *calendar.Component, error) {
		p := h.cache.get(col, etag)

		return p.card, p.cal, p.err
	}
}

// Birthdays returns the generated birthdays snapshot and a reader for its
// objects, or nil when the feature is off or there is no address book.
func (h *Handler) Birthdays() (*store.Snapshot, func(string) ([]byte, error)) {
	b := h.birthdaysCollection()
	if b == nil {
		return nil, nil
	}

	snap, err := b.Snapshot()
	if err != nil {
		return nil, nil
	}

	return snap, b.ReadBlob
}

type resKind int

const (
	resRoot resKind = iota
	resPrincipal
	resHome
	resCollection
	resObject
)

// resource is a resolved request path.
type resource struct {
	kind resKind
	home store.Kind
	col  collection
	// name is the collection name (resHome: requested child for MKCOL)
	// or the object name.
	name string
	obj  store.Object
	snap *store.Snapshot
	// exists is false for a resource that may be created (PUT, MKCOL).
	exists bool
}

func (h *Handler) href(parts ...string) string {
	var b strings.Builder

	b.WriteString(h.opts.Prefix + "/")

	for _, p := range parts {
		b.WriteString(url.PathEscape(p))
		b.WriteString("/")
	}

	return b.String()
}

func (h *Handler) principalHref() string { return h.href(h.store.Principal()) }

func (h *Handler) homeHref(k store.Kind) string { return h.href(h.store.Principal(), k.Home()) }

func (h *Handler) collectionHref(k store.Kind, name string) string {
	return h.href(h.store.Principal(), k.Home(), name)
}

func (h *Handler) objectHref(k store.Kind, col, name string) string {
	return h.collectionHref(k, col) + url.PathEscape(name)
}

func (h *Handler) resourceHref(r *resource) string {
	switch r.kind {
	case resRoot:
		return h.opts.Prefix + "/"
	case resPrincipal:
		return h.principalHref()
	case resHome:
		return h.homeHref(r.home)
	case resCollection:
		return h.collectionHref(r.home, r.col.Name())
	case resObject:
		return h.objectHref(r.home, r.col.Name(), r.name)
	}

	return h.opts.Prefix + "/"
}

var errNotFound = errors.New("not found")

// resolve maps a URL path to a resource.
func (h *Handler) resolve(p string) (*resource, error) {
	p = strings.TrimPrefix(p, h.opts.Prefix)

	var parts []string

	for s := range strings.SplitSeq(p, "/") {
		if s != "" {
			parts = append(parts, s)
		}
	}

	r := &resource{kind: resRoot, home: "", col: nil, name: "", obj: store.Object{}, snap: nil, exists: true}
	if len(parts) == 0 {
		return r, nil
	}

	if parts[0] != h.store.Principal() {
		return nil, errNotFound
	}

	r.kind = resPrincipal
	if len(parts) == 1 {
		return r, nil
	}

	switch parts[1] {
	case store.AddressBook.Home():
		r.home = store.AddressBook
	case store.Calendar.Home():
		r.home = store.Calendar
	default:
		return nil, errNotFound
	}

	r.kind = resHome
	if len(parts) == 2 {
		return r, nil
	}

	col := h.collection(r.home, parts[2])
	if col == nil {
		if len(parts) == 3 {
			r.name, r.exists = parts[2], false

			return r, nil
		}

		return nil, errNotFound
	}

	snap, err := col.Snapshot()
	if err != nil {
		return nil, err
	}

	r.kind, r.col, r.snap = resCollection, col, snap
	if len(parts) == 3 {
		return r, nil
	}

	if len(parts) > 4 {
		return nil, errNotFound
	}

	r.kind, r.name = resObject, parts[3]
	r.obj, r.exists = snap.Lookup(parts[3])

	return r, nil
}

func (h *Handler) collection(kind store.Kind, name string) collection {
	if c, err := h.store.Collection(kind, name); err == nil {
		return c
	}

	if kind == store.Calendar && name == birthdaysName {
		if b := h.birthdaysCollection(); b != nil {
			return b
		}
	}

	return nil
}

func (h *Handler) birthdaysCollection() *birthdays {
	if !h.opts.Birthdays {
		return nil
	}

	books := h.store.Collections(store.AddressBook)
	if len(books) == 0 {
		return nil
	}

	h.bdayMu.Lock()
	defer h.bdayMu.Unlock()

	if h.bday == nil || h.bday.book != books[0] {
		h.bday = newBirthdays(books[0], h.cache, calendar.BirthdayOptions{AlarmDays: h.opts.BirthdayAlarmDays, ProductID: ""}, h.opts.BirthdayColor)
	}

	return h.bday
}

// collections lists the collections of a home, real ones first.
func (h *Handler) collections(kind store.Kind) []collection {
	var out []collection

	for _, c := range h.store.Collections(kind) {
		out = append(out, c)
	}

	if kind == store.Calendar {
		if _, err := h.store.Collection(store.Calendar, birthdaysName); err != nil {
			if b := h.birthdaysCollection(); b != nil {
				out = append(out, b)
			}
		}
	}

	return out
}

// ServeHTTP dispatches DAV methods.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("DAV", "1, 3, access-control, addressbook, calendar-access, extended-mkcol")

	if strings.HasPrefix(r.URL.Path, "/.well-known/carddav") || strings.HasPrefix(r.URL.Path, "/.well-known/caldav") {
		http.Redirect(w, r, h.principalHref(), http.StatusMovedPermanently)

		return
	}

	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, REPORT, MKCOL, MKCALENDAR")
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		h.serveGet(w, r)
	case http.MethodPut:
		h.servePut(w, r)
	case http.MethodDelete:
		h.serveDelete(w, r)
	case "PROPFIND":
		h.servePropfind(w, r)
	case "PROPPATCH":
		h.serveProppatch(w, r)
	case "REPORT":
		h.serveReport(w, r)
	case "MKCOL", "MKCALENDAR":
		h.serveMkcol(w, r)
	default:
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, REPORT, MKCOL, MKCALENDAR")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errNotFound), errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrPrecondition):
		http.Error(w, "precondition failed", http.StatusPreconditionFailed)
	case errors.Is(err, store.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errBadXML):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		h.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (h *Handler) serveGet(w http.ResponseWriter, r *http.Request) {
	res, err := h.resolve(r.URL.Path)
	if err != nil {
		h.fail(w, r, err)

		return
	}

	switch {
	case res.kind == resObject && res.exists:
		h.serveObject(w, r, res)
	case res.kind == resCollection && !wantsHTML(r):
		h.serveExport(w, res)
	case wantsHTML(r) && h.opts.UIPath != "":
		http.Redirect(w, r, h.opts.UIPath, http.StatusFound)
	case res.kind != resObject:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "farvardin CardDAV/CalDAV server. Point your client at "+h.principalHref()+"\n")
	default:
		h.fail(w, r, errNotFound)
	}
}

// format picks a representation of a card from ?format= or Accept.
func format(r *http.Request) string {
	if f := r.URL.Query().Get("format"); f != "" {
		return strings.ToLower(f)
	}

	accept := strings.ToLower(r.Header.Get("Accept"))

	switch {
	case strings.Contains(accept, "application/jscontact+json"):
		return "jscontact"
	case strings.Contains(accept, "version=4.0"):
		return "4.0"
	case strings.Contains(accept, "version=3.0"):
		return "3.0"
	default:
		return ""
	}
}

func (h *Handler) serveObject(w http.ResponseWriter, r *http.Request, res *resource) {
	data, err := res.col.ReadBlob(res.obj.ETag)
	if err != nil {
		h.fail(w, r, err)

		return
	}

	etag := `"` + res.obj.ETag + `"`
	ctype := contentType(res.home, parse(res.home, data))

	if f := format(r); res.home == store.AddressBook && f != "" {
		data, ctype, err = convertCard(data, f)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotAcceptable)

			return
		}

		etag = `"` + res.obj.ETag + "-" + f + `"`
	}

	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)

		return
	}

	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))

	if r.Method == http.MethodHead {
		return
	}

	_, _ = w.Write(data) //nolint:gosec // vCard/iCalendar data served with its own content type, not HTML
}

var errUnknownFormat = errors.New("unknown format; use 3.0, 4.0 or jscontact")

func convertCard(data []byte, f string) ([]byte, string, error) {
	card, err := contact.Parse(data)
	if err != nil {
		return nil, "", err
	}

	switch f {
	case "3.0", "4.0":
		c, err := contact.Convert(card, f)
		if err != nil {
			return nil, "", err
		}

		return c.Encode(), "text/vcard; charset=utf-8; version=" + f, nil
	case "jscontact", "json":
		out, err := json.MarshalIndent(contact.ToJSContact(card, true), "", "  ")
		if err != nil {
			return nil, "", err //nolint:wrapcheck // the message goes to the client as it is
		}

		return out, "application/jscontact+json", nil
	default:
		return nil, "", errUnknownFormat
	}
}

// serveExport answers GET on a collection: every card as one .vcf, or
// every event as one .ics (a webcal feed).
func (h *Handler) serveExport(w http.ResponseWriter, res *resource) {
	if res.home == store.AddressBook {
		w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+res.col.Name()+`.vcf"`)

		for _, o := range res.snap.Objects {
			if data, err := res.col.ReadBlob(o.ETag); err == nil {
				_, _ = w.Write(data)
			}
		}

		return
	}

	merged := &calendar.Component{Name: "VCALENDAR", Props: nil, Components: nil}
	merged.Props = append(merged.Props, vline("VERSION", "2.0"), vline("PRODID", "-//farvardin//EN"), vline("X-WR-CALNAME", displayName(res.col, res.snap)))
	zones := map[string]bool{}

	for _, o := range res.snap.Objects {
		p := h.cache.get(res.col, o.ETag)
		if p.cal == nil {
			continue
		}

		for _, c := range p.cal.Components {
			if c.Name == "VTIMEZONE" {
				id := c.Value("TZID")
				if zones[id] {
					continue
				}

				zones[id] = true
			}

			merged.Components = append(merged.Components, c)
		}
	}

	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	_, _ = w.Write(merged.Encode())
}

func etagFrom(header string) string {
	header = strings.TrimSpace(header)
	if header == "*" {
		return "*"
	}

	if i := strings.IndexByte(header, ','); i > 0 {
		header = header[:i]
	}

	header = strings.TrimPrefix(strings.TrimSpace(header), "W/")

	return strings.Trim(header, `"`)
}

func requester(r *http.Request) string {
	if ua := r.UserAgent(); ua != "" {
		return ua
	}

	return "unknown"
}

// servePut stores an object after validating it.
func (h *Handler) servePut(w http.ResponseWriter, r *http.Request) {
	res, err := h.resolve(r.URL.Path)
	if err != nil {
		h.fail(w, r, err)

		return
	}

	if res.kind != resObject {
		http.Error(w, "PUT is only allowed on objects inside a collection", http.StatusMethodNotAllowed)

		return
	}

	col, ok := res.col.(writable)
	if !ok {
		writeError(w, http.StatusForbidden, el(NSDAV, "need-privileges"), "this collection is read-only")

		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, h.opts.MaxResourceSize+1))
	if err != nil {
		h.fail(w, r, err)

		return
	}

	if int64(len(body)) > h.opts.MaxResourceSize {
		h.metrics.Rejected("too-large")
		writeError(w, http.StatusRequestEntityTooLarge, el(nsFor(res.home), "max-resource-size"), "object is larger than max-resource-size")

		return
	}

	cond := store.Condition{IfMatch: etagFrom(r.Header.Get("If-Match")), IfNoneMatch: etagFrom(r.Header.Get("If-None-Match")) == "*"}

	var stored []byte

	if res.home == store.AddressBook {
		stored, ok = h.checkCard(w, res, body)
	} else {
		stored, ok = h.checkEvent(w, res, body)
	}

	if !ok {
		return
	}

	etag, created, err := col.Put(res.name, stored, cond, requester(r))
	if err != nil {
		h.fail(w, r, err)

		return
	}

	op := "modified"
	if created {
		op = "created"
	}

	h.metrics.Write(string(res.home), op)

	// RFC 4791 / 6352: send the ETag only when the stored bytes are
	// exactly what the client sent; otherwise it must fetch them again.
	if bytes.Equal(stored, body) {
		w.Header().Set("ETag", `"`+etag+`"`)
	}

	if created {
		w.WriteHeader(http.StatusCreated)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func nsFor(k store.Kind) string {
	if k == store.AddressBook {
		return NSCard
	}

	return NSCal
}

// checkCard validates an incoming vCard, applies the photo policy and
// returns the bytes to store. On failure it has written the response.
func (h *Handler) checkCard(w http.ResponseWriter, res *resource, body []byte) ([]byte, bool) {
	card, err := contact.Parse(body)
	if err != nil {
		h.metrics.Rejected("invalid-vcard")
		writeError(w, http.StatusForbidden, el(NSCard, "valid-address-data"), err.Error())

		return nil, false
	}

	if issues := contact.Validate(card); contact.HasErrors(issues) {
		h.metrics.Rejected("invalid-vcard")

		var msgs []string

		for _, i := range issues {
			if i.Severity == contact.Error {
				msgs = append(msgs, i.Message)
			}
		}

		writeError(w, http.StatusForbidden, el(NSCard, "valid-address-data"), strings.Join(msgs, "; "))

		return nil, false
	}

	if owner := h.cache.uidOwner(res.col, res.snap, card.UID(), res.name); owner != "" {
		h.metrics.Rejected("uid-conflict")
		writeError(w, http.StatusForbidden, el(NSCard, "no-uid-conflict", hrefEl(h.objectHref(res.home, res.col.Name(), owner))), "UID already used by "+owner)

		return nil, false
	}

	var prev *contact.Card
	if res.exists {
		prev = h.cache.get(res.col, res.obj.ETag).card
	}

	out, action := contact.ApplyPhotoPolicy(card, prev, h.opts.Photo)
	if action == contact.PhotoUnchanged {
		return body, true
	}

	h.metrics.Photo(string(action))
	h.log.Info("photo policy applied", "object", res.name, "action", action)

	return out.Encode(), true
}

func (h *Handler) checkEvent(w http.ResponseWriter, res *resource, body []byte) ([]byte, bool) {
	cal, err := calendar.Parse(body)
	if err != nil {
		h.metrics.Rejected("invalid-icalendar")
		writeError(w, http.StatusForbidden, el(NSCal, "valid-calendar-data"), err.Error())

		return nil, false
	}

	uid, err := calendar.Validate(cal, supportedComponents(res.snap.Meta()))
	if err != nil {
		h.metrics.Rejected("invalid-icalendar")

		pre := "valid-calendar-object-resource"
		if errors.Is(err, calendar.ErrNotSupported) {
			pre = "supported-calendar-component"
		}

		writeError(w, http.StatusForbidden, el(NSCal, pre), err.Error())

		return nil, false
	}

	if owner := h.cache.uidOwner(res.col, res.snap, uid, res.name); owner != "" {
		h.metrics.Rejected("uid-conflict")
		writeError(w, http.StatusForbidden, el(NSCal, "no-uid-conflict", hrefEl(h.objectHref(res.home, res.col.Name(), owner))), "UID already used by "+owner)

		return nil, false
	}

	return body, true
}

func supportedComponents(m store.Meta) []string {
	v := m.Get("", "components")
	if v == "" {
		return nil
	}

	return strings.Split(v, ",")
}

func (h *Handler) serveDelete(w http.ResponseWriter, r *http.Request) {
	res, err := h.resolve(r.URL.Path)
	if err != nil {
		h.fail(w, r, err)

		return
	}

	switch {
	case res.kind == resCollection:
		writeError(w, http.StatusForbidden, el(NSDAV, "need-privileges"), "deleting a whole collection is disabled; remove its directory on the server")
	case res.kind != resObject || !res.exists:
		h.fail(w, r, errNotFound)
	default:
		col, ok := res.col.(writable)
		if !ok {
			writeError(w, http.StatusForbidden, el(NSDAV, "need-privileges"), "this collection is read-only")

			return
		}

		cond := store.Condition{IfMatch: etagFrom(r.Header.Get("If-Match")), IfNoneMatch: false}
		if err := col.Delete(res.name, cond, requester(r)); err != nil {
			h.fail(w, r, err)

			return
		}

		h.metrics.Write(string(res.home), "deleted")
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Handler) serveMkcol(w http.ResponseWriter, r *http.Request) {
	res, err := h.resolve(r.URL.Path)
	if err != nil {
		h.fail(w, r, err)

		return
	}

	if res.kind != resHome || res.exists {
		writeError(w, http.StatusMethodNotAllowed, el(NSDAV, "resource-must-be-null"), "a collection can only be created inside contacts/ or calendars/")

		return
	}

	if r.Method == "MKCALENDAR" && res.home != store.Calendar {
		writeError(w, http.StatusForbidden, el(NSCal, "calendar-collection-location-ok"), "calendars live under calendars/")

		return
	}

	body, err := readXML(r)
	if err != nil {
		h.fail(w, r, err)

		return
	}

	var meta store.Meta

	if body != nil {
		if set := body.child(NSDAV, "set"); set != nil {
			if prop := set.child(NSDAV, "prop"); prop != nil {
				for _, p := range prop.Children {
					_ = applyProp(&meta, &p, false)
				}
			}
		}
	}

	if _, err := h.store.Create(res.home, res.name, meta, requester(r)); err != nil {
		if errors.Is(err, store.ErrExists) || errors.Is(err, store.ErrInvalidName) {
			http.Error(w, err.Error(), http.StatusMethodNotAllowed)

			return
		}

		h.fail(w, r, err)

		return
	}

	w.WriteHeader(http.StatusCreated)
}

func displayName(col collection, snap *store.Snapshot) string {
	if n := snap.Meta().DisplayName(); n != "" {
		return n
	}

	return path.Base(col.Name())
}
