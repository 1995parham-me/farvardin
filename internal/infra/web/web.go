// Package web serves the browser UI: a dashboard, contacts with their
// photos, change history and photo restore, an agenda, and a report of
// cards that fail validation. Pages are rendered on the server; a small
// script adds instant search and copy buttons.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/1995parham-me/farvardin/internal/domain/calendar"
	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Parser returns parsed objects; the DAV handler's cache implements it.
type Parser func(col *store.Collection, etag string) (*contact.Card, *calendar.Component, error)

// BirthdaySource returns the generated birthdays calendar, or nil.
type BirthdaySource func() (*store.Snapshot, func(string) ([]byte, error))

// CardWriter stores a card through the server's validated write path.
type CardWriter func(book, name string, data []byte, ifMatch, requester string) (string, error)

// Options configures the UI.
type Options struct {
	// Prefix is where the UI is mounted, for example "/ui".
	Prefix string
	// DAVPrefix is where the DAV tree is mounted, for download links.
	DAVPrefix string
	// Sessions enables the login page; nil leaves authentication to a
	// proxy in front.
	Sessions *Sessions
	// Writer enables the actions that change a card (photo restore).
	Writer CardWriter
	// Full parses an object with its photo bytes (the Parser returns slim
	// cards); thumbnails and restore need it.
	Full Parser
	// ThumbnailCacheBytes bounds the scaled-photo cache.
	ThumbnailCacheBytes int
	// PhoneRegion is the country of numbers written without a country
	// code; lint suggests their E.164 form and the contact page fixes them.
	PhoneRegion []string
}

// Handler is the UI http.Handler, mounted at Prefix.
type Handler struct {
	store     *store.Store
	parse     Parser
	full      Parser
	birthdays BirthdaySource
	write     CardWriter
	prefix    string
	davPrefix string
	region    []string
	sessions  *Sessions
	log       *slog.Logger
	tmpl      *template.Template
	mux       *http.ServeMux
	thumbs    *thumbCache
	lists     *listCache
}

// New builds the UI.
func New(s *store.Store, parse Parser, birthdays BirthdaySource, opts Options, log *slog.Logger) (*Handler, error) {
	h := &Handler{
		store: s, parse: parse, full: opts.Full, birthdays: birthdays, write: opts.Writer,
		prefix: strings.TrimSuffix(opts.Prefix, "/"), davPrefix: strings.TrimSuffix(opts.DAVPrefix, "/"), region: opts.PhoneRegion,
		sessions: opts.Sessions, log: log, tmpl: nil, mux: http.NewServeMux(),
		thumbs: newThumbCache(opts.ThumbnailCacheBytes), lists: newListCache(),
	}

	funcs := template.FuncMap{
		"ui":       func(p string) string { return h.prefix + p },
		"asset":    func(p string) string { return h.prefix + "/static/" + p + "?v=" + assetVersion },
		"initials": initials,
		"hue":      hue,
		"date":     func(t time.Time) string { return t.Format("Mon 2 Jan 2006") },
		"day":      func(t time.Time) string { return t.Format("2 Jan") },
		"clock":    func(t time.Time) string { return t.Local().Format("15:04") },            //nolint:gosmopolitan // the UI shows the server's local time on purpose
		"datetime": func(t time.Time) string { return t.Local().Format("2 Jan 2006 15:04") }, //nolint:gosmopolitan // as above
		"ago":      ago,
		"short":    func(s string) string { return s[:min(8, len(s))] },
		"kib":      func(n int64) string { return strconv.FormatInt((n+1023)/1024, 10) + " KiB" },
		"join":     strings.Join,
		"device":   Device,
		"pct":      func(a, b int) int { return a * 100 / max(b, 1) },
		"tenth":    func(a, b int) int { return (a*10 + max(b, 1) - 1) / max(b, 1) },
	}

	tmpl, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parsing templates: %w", err)
	}

	h.tmpl = tmpl

	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("static assets: %w", err)
	}

	p := h.prefix
	h.mux.Handle("GET "+p+"/static/", cacheForever(http.StripPrefix(p+"/static/", http.FileServerFS(static))))
	h.mux.HandleFunc("GET "+p+"/login", h.loginPage)
	h.mux.HandleFunc("POST "+p+"/login", h.login)
	h.mux.HandleFunc("POST "+p+"/logout", h.logout)
	h.mux.HandleFunc("GET "+p+"/{$}", h.dashboard)
	h.mux.HandleFunc("GET "+p+"/contacts/{book}/{$}", h.contacts)
	h.mux.HandleFunc("GET "+p+"/contacts/{book}/{name}", h.contactPage)
	h.mux.HandleFunc("POST "+p+"/contacts/{book}/{name}/photo", h.restorePhoto)
	h.mux.HandleFunc("POST "+p+"/contacts/{book}/{name}/phones", h.fixPhones)
	h.mux.HandleFunc("GET "+p+"/photo/{book}/{name}", h.photo)
	h.mux.HandleFunc("GET "+p+"/calendars/{cal}/{$}", h.events)
	h.mux.HandleFunc("GET "+p+"/lint", h.lint)

	return h, nil
}

// ServeHTTP checks the session (when login is on) and serves the page.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil || h.public(r.URL.Path) {
		h.mux.ServeHTTP(w, r)

		return
	}

	user, err := h.sessions.User(r)
	if err != nil {
		http.Redirect(w, r, h.prefix+"/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)

		return
	}

	h.mux.ServeHTTP(w, r.WithContext(withUser(r.Context(), user)))
}

// public paths need no session.
func (h *Handler) public(p string) bool {
	return p == h.prefix+"/login" || strings.HasPrefix(p, h.prefix+"/static/")
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	h.renderStatus(w, r, http.StatusOK, name, data)
}

func (h *Handler) renderStatus(w http.ResponseWriter, r *http.Request, status int, name string, data map[string]any) {
	data["User"] = userFrom(r.Context())
	data["Login"] = h.sessions != nil
	data["Prefix"] = h.prefix
	data["Books"] = h.store.Collections(store.AddressBook)
	data["Calendars"] = h.calendarNames()
	data["Path"] = r.URL.Path

	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		h.log.Error("rendering page", "template", name, "error", err)
		http.Error(w, "rendering failed", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (h *Handler) calendarNames() []string {
	var out []string

	for _, c := range h.store.Collections(store.Calendar) {
		out = append(out, c.Name())
	}

	if snap, _ := h.birthdays(); snap != nil && !slices.Contains(out, "birthdays") {
		out = append(out, "birthdays")
	}

	return out
}

// thumbSize is the longest side that counts as a low-resolution photo.
const thumbSize = 128

// person is one row of a contact list.
type person struct {
	Name     string
	File     string
	ETag     string
	Book     string
	Org      string
	Email    string
	Phone    string
	Letter   string
	Search   string
	HasPhoto bool
	LowRes   bool
	PhotoW   int
	Issues   int
}

func (h *Handler) people(book *store.Collection) ([]person, *store.Snapshot, error) {
	snap, err := book.Snapshot()
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", book.Name(), err)
	}

	if cached, ok := h.lists.get(book.Name(), snap.Token); ok {
		return cached, snap, nil
	}

	out := make([]person, 0, len(snap.Objects))

	for _, o := range snap.Objects {
		p := person{
			Name: o.Name, File: o.Name, ETag: o.ETag, Book: book.Name(), Org: "", Email: "", Phone: "",
			Letter: "#", Search: strings.ToLower(o.Name), HasPhoto: false, LowRes: false, PhotoW: 0, Issues: 1,
		}

		card, _, err := h.parse(book, o.ETag)
		if err != nil || card == nil {
			out = append(out, p)

			continue
		}

		p.Name, p.Org, p.Issues = card.DisplayName(), card.Organization(), 0

		if e := card.Values("EMAIL"); len(e) > 0 {
			p.Email = e[0].Value
		}

		if t := card.Values("TEL"); len(t) > 0 {
			p.Phone = t[0].Value
		}

		if ph, ok := card.PhotoInfo(); ok {
			p.HasPhoto = true
			p.PhotoW = max(ph.Width, ph.Height)
			p.LowRes = p.PhotoW > 0 && p.PhotoW <= thumbSize
		}

		for _, i := range contact.Validate(*card) {
			if i.Severity == contact.Error {
				p.Issues++
			}
		}

		p.Letter = letter(p.Name)
		words := []string{p.Name, p.Org, p.Email, p.Phone, card.Value("NICKNAME")}
		for _, t := range card.Values("TEL") {
			words = append(words, contact.PhoneKeys(t.Value, h.region)...)
		}

		p.Search = strings.ToLower(strings.Join(words, " "))
		out = append(out, p)
	}

	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	h.lists.put(book.Name(), snap.Token, out)

	return out, snap, nil
}

// letter is the index letter of a name: an upper-case Latin letter, the
// first Persian letter, or "#".
func letter(name string) string {
	for _, r := range name {
		if unicode.IsLetter(r) {
			return string(unicode.ToUpper(r))
		}

		if !unicode.IsSpace(r) && !unicode.IsPunct(r) {
			break
		}
	}

	return "#"
}

type group struct {
	Letter string
	People []person
}

func (h *Handler) contacts(w http.ResponseWriter, r *http.Request) {
	book, err := h.store.Collection(store.AddressBook, r.PathValue("book"))
	if err != nil {
		http.NotFound(w, r)

		return
	}

	all, snap, err := h.people(book)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	filter := r.URL.Query().Get("f")
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	qd := phoneQuery(q)
	counts := map[string]int{"all": len(all)}

	var shown []person

	for _, p := range all {
		counts[map[bool]string{true: "photo", false: "nophoto"}[p.HasPhoto]]++

		if p.LowRes {
			counts["lowres"]++
		}

		if p.Issues > 0 {
			counts["errors"]++
		}

		keep := filter == "" || filter == "all" ||
			(filter == "photo" && p.HasPhoto) || (filter == "nophoto" && !p.HasPhoto) ||
			(filter == "lowres" && p.LowRes) || (filter == "errors" && p.Issues > 0)

		if keep && (q == "" || strings.Contains(p.Search, q) || (len(qd) >= 4 && strings.Contains(p.Search, qd))) {
			shown = append(shown, p)
		}
	}

	var (
		groups  []group
		letters []string
	)

	for _, p := range shown {
		if len(groups) == 0 || groups[len(groups)-1].Letter != p.Letter {
			groups = append(groups, group{Letter: p.Letter, People: nil})
			letters = append(letters, p.Letter)
		}

		groups[len(groups)-1].People = append(groups[len(groups)-1].People, p)
	}

	h.render(w, r, "contacts.html", map[string]any{
		"Title": snap.Meta().DisplayName(), "Book": book.Name(), "Groups": groups, "Letters": letters,
		"Shown": len(shown), "Counts": counts, "Filter": filter, "Query": r.URL.Query().Get("q"),
		"Export": h.davPrefix + "/" + h.store.Principal() + "/contacts/" + book.Name() + "/",
	})
}

type field struct {
	Label string
	Value string
	Link  template.URL
	Copy  bool
	Kind  string
}

func (h *Handler) contactPage(w http.ResponseWriter, r *http.Request) {
	book, err := h.store.Collection(store.AddressBook, r.PathValue("book"))
	if err != nil {
		http.NotFound(w, r)

		return
	}

	name := r.PathValue("name")

	snap, err := book.Snapshot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	obj, ok := snap.Lookup(name)
	if !ok {
		http.NotFound(w, r)

		return
	}

	card, _, err := h.parse(book, obj.ETag)
	if err != nil || card == nil {
		http.Error(w, fmt.Sprintf("card does not parse: %v", err), http.StatusUnprocessableEntity)

		return
	}

	revs, _ := book.History(name, 25)

	photoInfo := ""
	if p, ok := card.PhotoInfo(); ok {
		photoInfo = fmt.Sprintf("%d×%d · %d KiB", p.Width, p.Height, p.Bytes/1024)
	}

	_, hasPhoto := card.Get("PHOTO")
	_, phoneFixes := contact.FixPhones(*card, h.region)

	h.render(w, r, "contact.html", map[string]any{
		"Title": card.DisplayName(), "Book": book.Name(), "File": name, "Card": card, "Fields": fields(*card, h.region),
		"Org": strings.Join(nonEmpty(orgParts(*card)), " · "), "JobTitle": card.Value("TITLE"),
		"Issues": contact.Lint(*card, h.region), "PhoneFixes": phoneFixes, "PhonesFixed": r.URL.Query().Get("phones"), "Timeline": h.timeline(book, revs), "Photos": h.photoHistory(book, *card, revs),
		"Object": obj, "HasPhoto": hasPhoto, "PhotoInfo": photoInfo, "Version": card.Version(), "CanWrite": h.write != nil,
		"Restored": r.URL.Query().Get("restored") != "", "Failed": r.URL.Query().Get("error"),
		"Raw": h.davPrefix + "/" + h.store.Principal() + "/contacts/" + book.Name() + "/" + name,
	})
}

// fields lists a card's properties in reading order with labels.
func fields(c contact.Card, region []string) []field {
	var out []field

	label := func(v contact.Value, fallback string) string {
		parts := slices.Clone(v.Types)
		if v.Label != "" {
			parts = []string{v.Label}
		}

		parts = slices.DeleteFunc(parts, func(s string) bool { return s == "internet" || s == "voice" || s == "pref" })
		if len(parts) == 0 {
			return fallback
		}

		return fallback + " · " + strings.Join(parts, ", ")
	}

	add := func(f field) {
		if strings.TrimSpace(f.Value) != "" {
			out = append(out, f)
		}
	}

	for _, v := range c.Values("TEL") {
		add(field{Label: label(v, "Phone"), Value: v.Value, Link: telLink(v.Value, region), Copy: true, Kind: "phone"})
	}

	for _, v := range c.Values("EMAIL") {
		add(field{Label: label(v, "Email"), Value: v.Value, Link: safeLink("mailto:" + v.Value), Copy: true, Kind: "email"})
	}

	add(field{Label: "Nickname", Value: c.Value("NICKNAME"), Link: "", Copy: false, Kind: "text"})

	for _, v := range c.Values("ADR") {
		add(field{Label: label(v, "Address"), Value: v.Value, Link: "", Copy: true, Kind: "address"})
	}

	for _, v := range c.Values("URL") {
		add(field{Label: label(v, "Link"), Value: v.Value, Link: safeLink(v.Value), Copy: false, Kind: "link"})
	}

	for _, n := range []string{"SOCIALPROFILE", "X-SOCIALPROFILE", "IMPP"} {
		for _, l := range c.All(n) {
			svc := strings.Join(append(l.Param("SERVICE-TYPE"), l.Param("TYPE")...), ", ")
			add(field{Label: "Profile · " + svc, Value: l.Text(), Link: safeLink(l.Text()), Copy: false, Kind: "link"})
		}
	}

	for _, d := range c.Dates() {
		val := fmt.Sprintf("%d %s", d.Day, d.Month)
		if d.Year > 0 {
			val += " " + strconv.Itoa(d.Year)
		}

		lbl := strings.ToUpper(string(d.Kind[:1])) + string(d.Kind[1:])
		if d.Label != "" && d.Kind == contact.OtherDate {
			lbl = d.Label
		}

		add(field{Label: lbl, Value: val, Link: "", Copy: false, Kind: "date"})
	}

	add(field{Label: "Pronouns", Value: c.Value("PRONOUNS"), Link: "", Copy: false, Kind: "text"})
	add(field{Label: "Note", Value: c.Value("NOTE"), Link: "", Copy: false, Kind: "note"})

	return out
}

// telLink is the tel: URI of a number: E.164 when it can be read, else
// its digits. html/template refuses tel: in a plain string, so the URI is
// built from digits only and then marked safe.
func telLink(v string, region []string) template.URL {
	n, ok := contact.NormalizeTel(v, region)
	if !ok {
		n = strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' || r == '+' || r == '*' || r == '#' {
				return r
			}

			return -1
		}, strings.TrimPrefix(strings.ToLower(strings.TrimSpace(v)), "tel:"))
	}

	if n == "" {
		return ""
	}

	return template.URL("tel:" + strings.ReplaceAll(n, "#", "%23")) //nolint:gosec // digits, +, * and an escaped # only
}

// safeLink keeps the http, https and mailto links of a card and drops the
// rest (javascript: and the like); it matches what html/template allows.
func safeLink(v string) template.URL {
	v = strings.TrimSpace(v)
	if u, err := url.Parse(v); err == nil && (u.Scheme == "" || u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "mailto") {
		return template.URL(v) //nolint:gosec // scheme checked above
	}

	return ""
}

func orgParts(c contact.Card) []string {
	l, ok := c.Get("ORG")
	if !ok {
		return nil
	}

	return l.Components()
}

func nonEmpty(in []string) []string {
	return slices.DeleteFunc(in, func(s string) bool { return strings.TrimSpace(s) == "" })
}

func (h *Handler) lint(w http.ResponseWriter, r *http.Request) {
	type finding struct {
		Book, File, Name string
		Issues           []contact.Issue
	}

	var findings []finding

	for _, book := range h.store.Collections(store.AddressBook) {
		snap, err := book.Snapshot()
		if err != nil {
			continue
		}

		for _, o := range snap.Objects {
			card, _, err := h.parse(book, o.ETag)
			if err != nil || card == nil {
				findings = append(findings, finding{Book: book.Name(), File: o.Name, Name: o.Name, Issues: []contact.Issue{{Severity: contact.Error, Message: fmt.Sprint(err), Fix: ""}}})

				continue
			}

			if issues := contact.Lint(*card, h.region); len(issues) > 0 {
				findings = append(findings, finding{Book: book.Name(), File: o.Name, Name: card.DisplayName(), Issues: issues})
			}
		}
	}

	h.render(w, r, "lint.html", map[string]any{"Title": "Lint", "Findings": findings})
}

// phoneQuery returns the digits of a search that looks like a phone
// number ("0912 345", "+98-912"), or "".
func phoneQuery(q string) string {
	if q == "" || strings.Trim(q, "0123456789 +-().") != "" {
		return ""
	}

	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}

		return -1
	}, q)
}

func initials(name string) string {
	var out []rune

	for f := range strings.FieldsSeq(name) {
		r := []rune(f)[0]
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out = append(out, unicode.ToUpper(r))
		}

		if len(out) == 2 {
			break
		}
	}

	if len(out) == 0 {
		return "?"
	}

	return string(out)
}

func hue(s string) int {
	f := fnv.New32a()
	_, _ = f.Write([]byte(s))

	return int(f.Sum32() % 12)
}

func ago(t time.Time) string {
	d := time.Since(t)

	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h ago"
	case d < 30*24*time.Hour:
		return strconv.Itoa(int(d.Hours()/24)) + " d ago"
	default:
		return t.Format("2 Jan 2006")
	}
}
