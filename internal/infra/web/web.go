// Package web serves a read-only browser UI for the store: contacts with
// their photos and history, upcoming birthdays and events, and a report
// of cards that fail validation.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
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

// Handler is the UI http.Handler, mounted at Prefix.
type Handler struct {
	store     *store.Store
	parse     Parser
	birthdays BirthdaySource
	prefix    string
	davPrefix string
	log       *slog.Logger
	tmpl      *template.Template
	mux       *http.ServeMux
}

// New builds the UI. prefix is where it is mounted (for example "/ui").
func New(s *store.Store, parse Parser, birthdays BirthdaySource, prefix, davPrefix string, log *slog.Logger) (*Handler, error) {
	h := &Handler{store: s, parse: parse, birthdays: birthdays, prefix: strings.TrimSuffix(prefix, "/"), davPrefix: strings.TrimSuffix(davPrefix, "/"), log: log, tmpl: nil, mux: http.NewServeMux()}

	funcs := template.FuncMap{
		"ui":       func(p string) string { return h.prefix + p },
		"initials": initials,
		"hue":      hue,
		"date":     func(t time.Time) string { return t.Format("Mon 2 Jan 2006") },
		"datetime": func(t time.Time) string { return t.Local().Format("2 Jan 2006 15:04") }, //nolint:gosmopolitan // the UI shows the server's local time on purpose
		"short":    func(s string) string { return s[:min(8, len(s))] },
		"kib":      func(n int64) string { return strconv.FormatInt((n+1023)/1024, 10) + " KiB" },
		"join":     strings.Join,
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

	h.mux.Handle("GET "+h.prefix+"/static/", http.StripPrefix(h.prefix+"/static/", http.FileServerFS(static)))
	h.mux.HandleFunc("GET "+h.prefix+"/{$}", h.dashboard)
	h.mux.HandleFunc("GET "+h.prefix+"/contacts/{book}/{$}", h.contacts)
	h.mux.HandleFunc("GET "+h.prefix+"/contacts/{book}/{name}", h.contactPage)
	h.mux.HandleFunc("GET "+h.prefix+"/photo/{book}/{name}", h.photo)
	h.mux.HandleFunc("GET "+h.prefix+"/calendars/{cal}/{$}", h.events)
	h.mux.HandleFunc("GET "+h.prefix+"/lint", h.lint)

	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) render(w http.ResponseWriter, name string, data map[string]any) {
	data["Prefix"] = h.prefix
	data["Books"] = h.store.Collections(store.AddressBook)
	data["Calendars"] = h.calendarNames()

	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		h.log.Error("rendering page", "template", name, "error", err)
		http.Error(w, "rendering failed", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'")
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

// person is one row of a contact list.
type person struct {
	Name     string
	File     string
	Book     string
	Org      string
	Email    string
	Phone    string
	HasPhoto bool
	Issues   int
}

func (h *Handler) people(book *store.Collection) ([]person, *store.Snapshot, error) {
	snap, err := book.Snapshot()
	if err != nil {
		return nil, nil, err
	}

	out := make([]person, 0, len(snap.Objects))

	for _, o := range snap.Objects {
		card, _, err := h.parse(book, o.ETag)
		if err != nil || card == nil {
			out = append(out, person{Name: o.Name, File: o.Name, Book: book.Name(), Org: "", Email: "", Phone: "", HasPhoto: false, Issues: 1})

			continue
		}

		p := person{Name: card.DisplayName(), File: o.Name, Book: book.Name(), Org: card.Organization(), Email: "", Phone: "", HasPhoto: false, Issues: 0}

		if e := card.Values("EMAIL"); len(e) > 0 {
			p.Email = e[0].Value
		}

		if t := card.Values("TEL"); len(t) > 0 {
			p.Phone = t[0].Value
		}

		_, p.HasPhoto = card.Get("PHOTO")

		for _, i := range contact.Validate(*card) {
			if i.Severity == contact.Error {
				p.Issues++
			}
		}

		out = append(out, p)
	}

	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })

	return out, snap, nil
}

type upcoming struct {
	When  time.Time
	Days  int
	Title string
	Link  string
}

func (h *Handler) dashboard(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()

	type colInfo struct {
		Kind, Name, Display, Link string
		Objects                   int
	}

	var (
		cols   []colInfo
		recent []map[string]any
		soon   []upcoming
	)

	for _, kind := range []store.Kind{store.AddressBook, store.Calendar} {
		for _, c := range h.store.Collections(kind) {
			snap, err := c.Snapshot()
			if err != nil {
				continue
			}

			link := h.prefix + "/" + kind.Home() + "/" + c.Name() + "/"
			cols = append(cols, colInfo{Kind: string(kind), Name: c.Name(), Display: snap.Meta().DisplayName(), Link: link, Objects: len(snap.Objects)})

			revs, _ := c.Recent(8)
			for _, r := range revs {
				recent = append(recent, map[string]any{"Rev": r, "Collection": c.Name()})
			}

			if kind != store.AddressBook {
				continue
			}

			for _, o := range snap.Objects {
				card, _, err := h.parse(c, o.ETag)
				if err != nil || card == nil {
					continue
				}

				for _, d := range card.Dates() {
					if d.Kind == contact.OtherDate {
						continue
					}

					next := d.Next(now)
					days := int(next.Sub(truncate(now)).Hours() / 24)

					if days <= 30 {
						title := card.DisplayName()
						if d.Kind == contact.Birthday {
							title = "🎂 " + title
							if d.Year > 0 {
								title += " turns " + strconv.Itoa(next.Year()-d.Year)
							}
						} else {
							title = "💍 " + title
						}

						soon = append(soon, upcoming{When: next, Days: days, Title: title, Link: h.prefix + "/contacts/" + c.Name() + "/" + o.Name})
					}
				}
			}
		}
	}

	sort.Slice(soon, func(i, j int) bool { return soon[i].When.Before(soon[j].When) })
	sort.Slice(recent, func(i, j int) bool {
		a, _ := recent[i]["Rev"].(store.Revision)
		b, _ := recent[j]["Rev"].(store.Revision)

		return a.When.After(b.When)
	})

	h.render(w, "dashboard.html", map[string]any{"Title": "Farvardin", "Collections": cols, "Soon": soon, "Recent": recent[:min(12, len(recent))]})
}

func truncate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
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

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	shown := all

	if q != "" {
		needle := strings.ToLower(q)
		shown = nil

		for _, p := range all {
			if strings.Contains(strings.ToLower(p.Name+" "+p.Org+" "+p.Email+" "+p.Phone), needle) {
				shown = append(shown, p)
			}
		}
	}

	h.render(w, "contacts.html", map[string]any{
		"Title": snap.Meta().DisplayName(), "Book": book.Name(), "People": shown, "Total": len(all), "Query": q,
		"Export": h.davPrefix + "/" + h.store.Principal() + "/contacts/" + book.Name() + "/",
	})
}

type field struct {
	Label string
	Value string
	Link  string
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

	hist, _ := book.History(name, 15)

	js, err := json.MarshalIndent(contact.ToJSContact(*card, false), "", "  ")
	if err != nil {
		js = []byte(err.Error())
	}
	_, hasPhoto := card.Get("PHOTO")

	photoInfo := ""
	if p, err := card.Photo(); err == nil {
		photoInfo = fmt.Sprintf("%d×%d %s, %d KiB", p.Width, p.Height, p.MediaType, len(p.Data)/1024)
	}

	h.render(w, "contact.html", map[string]any{
		"Title": card.DisplayName(), "Book": book.Name(), "File": name, "Card": card, "Fields": fields(*card),
		"Issues": contact.Validate(*card), "History": hist, "JSContact": string(js), "Object": obj,
		"HasPhoto": hasPhoto, "PhotoInfo": photoInfo, "Version": card.Version(),
		"Raw": h.davPrefix + "/" + h.store.Principal() + "/contacts/" + book.Name() + "/" + name,
	})
}

// fields lists a card's properties in reading order with labels.
func fields(c contact.Card) []field {
	var out []field

	label := func(v contact.Value, fallback string) string {
		parts := slices.Clone(v.Types)
		if v.Label != "" {
			parts = []string{v.Label}
		}

		parts = slices.DeleteFunc(parts, func(s string) bool { return s == "internet" || s == "voice" })
		if len(parts) == 0 {
			return fallback
		}

		return fallback + " · " + strings.Join(parts, ", ")
	}

	add := func(lbl, val, link string) {
		if strings.TrimSpace(val) != "" {
			out = append(out, field{Label: lbl, Value: val, Link: link})
		}
	}

	add("Nickname", c.Value("NICKNAME"), "")
	add("Organization", strings.Join(nonEmpty(orgParts(c)), " · "), "")
	add("Title", c.Value("TITLE"), "")

	for _, v := range c.Values("TEL") {
		add(label(v, "Phone"), v.Value, "tel:"+v.Value)
	}

	for _, v := range c.Values("EMAIL") {
		add(label(v, "Email"), v.Value, "mailto:"+v.Value)
	}

	for _, v := range c.Values("ADR") {
		add(label(v, "Address"), v.Value, "")
	}

	for _, v := range c.Values("URL") {
		add(label(v, "Link"), v.Value, v.Value)
	}

	for _, n := range []string{"SOCIALPROFILE", "X-SOCIALPROFILE", "IMPP"} {
		for _, l := range c.All(n) {
			svc := strings.Join(append(l.Param("SERVICE-TYPE"), l.Param("TYPE")...), ", ")
			add("Profile · "+svc, l.Text(), l.Text())
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

		add(lbl, val, "")
	}

	add("Pronouns", c.Value("PRONOUNS"), "")
	add("Note", c.Value("NOTE"), "")

	return out
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

func (h *Handler) photo(w http.ResponseWriter, r *http.Request) {
	book, err := h.store.Collection(store.AddressBook, r.PathValue("book"))
	if err != nil {
		http.NotFound(w, r)

		return
	}

	snap, err := book.Snapshot()
	if err != nil {
		http.NotFound(w, r)

		return
	}

	obj, ok := snap.Lookup(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)

		return
	}

	size, _ := strconv.Atoi(r.URL.Query().Get("s"))
	etag := `"` + obj.ETag + "-" + strconv.Itoa(size) + `"`

	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)

		return
	}

	card, _, err := h.parse(book, obj.ETag)
	if err != nil || card == nil {
		http.NotFound(w, r)

		return
	}

	p, err := card.Photo()
	if err != nil {
		http.NotFound(w, r)

		return
	}

	data, media, err := p.Thumbnail(size)
	if err != nil {
		data, media = p.Data, p.MediaType
	}

	w.Header().Set("Content-Type", media)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

type eventRow struct {
	When     time.Time
	AllDay   bool
	Title    string
	Location string
	Repeats  bool
	Kind     string
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("cal")
	now := truncate(time.Now())
	horizon := now.AddDate(0, 0, 90)

	var (
		rows  []eventRow
		title = name
	)

	collect := func(read func(string) ([]byte, error), snap *store.Snapshot, parse func(store.Object) *calendar.Component) {
		for _, o := range snap.Objects {
			cal := parse(o)
			if cal == nil {
				continue
			}

			s, ok := calendar.Summarize(cal)
			if !ok {
				continue
			}

			next, _ := calendar.NextOccurrence(cal, now)
			if next.Before(now) || next.After(horizon) {
				continue
			}

			rows = append(rows, eventRow{When: next, AllDay: s.AllDay, Title: s.Title, Location: s.Location, Repeats: s.Repeats, Kind: s.Kind})
		}

		_ = read
	}

	if col, err := h.store.Collection(store.Calendar, name); err == nil {
		snap, err := col.Snapshot()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		if d := snap.Meta().DisplayName(); d != "" {
			title = d
		}

		collect(nil, snap, func(o store.Object) *calendar.Component {
			_, cal, _ := h.parse(col, o.ETag)

			return cal
		})
	} else if snap, read := h.birthdays(); name == "birthdays" && snap != nil {
		title = "Birthdays"

		collect(read, snap, func(o store.Object) *calendar.Component {
			data, err := read(o.ETag)
			if err != nil {
				return nil
			}

			cal, _ := calendar.Parse(data)

			return cal
		})
	} else {
		http.NotFound(w, r)

		return
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].When.Before(rows[j].When) })

	h.render(w, "events.html", map[string]any{
		"Title": title, "Rows": rows,
		"Feed": h.davPrefix + "/" + h.store.Principal() + "/calendars/" + name + "/",
	})
}

func (h *Handler) lint(w http.ResponseWriter, _ *http.Request) {
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
				findings = append(findings, finding{Book: book.Name(), File: o.Name, Name: o.Name, Issues: []contact.Issue{{Severity: contact.Error, Message: fmt.Sprint(err)}}})

				continue
			}

			if issues := contact.Validate(*card); len(issues) > 0 {
				findings = append(findings, finding{Book: book.Name(), File: o.Name, Name: card.DisplayName(), Issues: issues})
			}
		}
	}

	h.render(w, "lint.html", map[string]any{"Title": "Lint", "Findings": findings})
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
