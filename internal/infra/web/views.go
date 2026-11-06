package web

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/calendar"
	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

var (
	iosAgent   = regexp.MustCompile(`^iOS/([\d.]+)`)
	macAgent   = regexp.MustCompile(`^(?:macOS|Mac OS X)/([\d.]+)`)
	davx5Agent = regexp.MustCompile(`^DAVx5/([\d.]+)`)
)

// Device turns a commit's Requester (the client's User-Agent) into a
// short label. An empty requester is a commit made with git directly.
func Device(requester string) string {
	switch {
	case requester == "":
		return "git"
	case iosAgent.MatchString(requester):
		return "iPhone · iOS " + iosAgent.FindStringSubmatch(requester)[1]
	case macAgent.MatchString(requester):
		return "Mac · macOS " + macAgent.FindStringSubmatch(requester)[1]
	case davx5Agent.MatchString(requester):
		return "Android · DAVx5 " + davx5Agent.FindStringSubmatch(requester)[1]
	case strings.HasPrefix(requester, "farvardin web UI"):
		return "Web UI"
	case strings.HasPrefix(requester, "farvardin MCP ("):
		return "MCP · " + strings.TrimSuffix(strings.TrimPrefix(requester, "farvardin MCP ("), ")")
	case strings.HasPrefix(requester, "farvardin"):
		return "farvardin"
	case strings.HasPrefix(requester, "curl/"):
		return "GitHub sync"
	case strings.Contains(requester, "Thunderbird"):
		return "Thunderbird"
	}

	name, _, _ := strings.Cut(requester, " ")

	return name
}

// friendly names for the properties a change touched; metadata that every
// save rewrites is left out.
var friendly = map[string]string{
	"FN": "name", "N": "name", "NICKNAME": "nickname", "TEL": "phone", "EMAIL": "email",
	"ADR": "address", "ORG": "organization", "TITLE": "title", "ROLE": "role", "NOTE": "note",
	"BDAY": "birthday", "ANNIVERSARY": "anniversary", "X-ABDATE": "date", "URL": "link",
	"PHOTO": "photo", "X-SOCIALPROFILE": "profile", "SOCIALPROFILE": "profile", "IMPP": "chat",
	"CATEGORIES": "groups", "X-ADDRESSING-GRAMMAR": "pronouns (iOS)", "PRONOUNS": "pronouns",
	"X-ABRELATEDNAMES": "related", "RELATED": "related", "X-SHARED-PHOTO-DISPLAY-PREF": "photo sharing",
}

var metadata = map[string]bool{
	"BEGIN": true, "END": true, "VERSION": true, "PRODID": true, "REV": true, "UID": true,
	"X-IMAGEHASH": true, "X-IMAGETYPE": true, "X-ABLABEL": true,
}

// entry is one revision in a contact's timeline.
type entry struct {
	Rev     store.Revision
	Device  string
	Summary string
	Kind    string // "add", "edit", "delete", "noop"
}

// timeline describes what each revision changed, by comparing it with
// the revision before it.
func (h *Handler) timeline(book *store.Collection, revs []store.Revision) []entry {
	out := make([]entry, 0, len(revs))

	for i, rev := range revs {
		e := entry{Rev: rev, Device: Device(rev.Requester), Summary: "", Kind: "edit"}

		switch {
		case rev.ETag == "":
			e.Summary, e.Kind = "deleted", "delete"
		case i == len(revs)-1:
			e.Summary, e.Kind = "created", "add"
		default:
			e.Summary, e.Kind = h.describe(book, revs[i+1].ETag, rev.ETag)
		}

		out = append(out, e)
	}

	return out
}

func (h *Handler) describe(book *store.Collection, before, after string) (string, string) {
	if before == "" {
		return "created again", "add"
	}

	a, _, errA := h.parse(book, before)
	b, _, errB := h.parse(book, after)

	if errA != nil || errB != nil || a == nil || b == nil {
		return "changed", "edit"
	}

	values := func(c *contact.Card) map[string][]string {
		m := map[string][]string{}

		for _, l := range c.Lines {
			switch {
			case metadata[l.Name]:
			case l.Name == "PHOTO":
				// Slim cards keep the photo's identity, not its bytes.
				info, _ := c.PhotoInfo()
				m[l.Name] = append(m[l.Name], info.Key)
			default:
				m[l.Name] = append(m[l.Name], l.String()[len(l.Group):])
			}
		}

		for k := range m {
			slices.Sort(m[k])
		}

		return m
	}

	va, vb := values(a), values(b)

	var parts []string

	seen := map[string]bool{}

	note := func(name, what string) {
		label := friendly[name]
		if label == "" {
			label = strings.ToLower(name)
		}

		if !seen[label+what] {
			seen[label+what] = true
			parts = append(parts, label+" "+what)
		}
	}

	for name, vals := range vb {
		old, ok := va[name]

		switch {
		case !ok:
			note(name, "added")
		case !slices.Equal(old, vals):
			if name == "PHOTO" {
				parts = append(parts, photoChange(*a, *b))
				seen["photo changed"] = true

				continue
			}

			note(name, "changed")
		}
	}

	for name := range va {
		if _, ok := vb[name]; !ok {
			note(name, "removed")
		}
	}

	if len(parts) == 0 {
		return "saved again, nothing changed", "noop"
	}

	sort.Strings(parts)

	return strings.Join(parts, ", "), "edit"
}

func photoChange(a, b contact.Card) string {
	pa, okA := a.PhotoInfo()
	pb, okB := b.PhotoInfo()

	switch {
	case !okA || !okB:
		return "photo changed"
	case pa.Width == pb.Width && pa.Height == pb.Height:
		return fmt.Sprintf("photo re-encoded (%d×%d)", pb.Width, pb.Height)
	default:
		return fmt.Sprintf("photo %d×%d → %d×%d", pa.Width, pa.Height, pb.Width, pb.Height)
	}
}

// photoVersion is one distinct photo a card has had.
type photoVersion struct {
	ETag    string // blob id of a revision that had it
	Since   time.Time
	Device  string
	Width   int
	Height  int
	Current bool
	Better  bool // larger than the current photo
}

// photoHistory lists the distinct photos in a card's history, newest
// first.
func (h *Handler) photoHistory(book *store.Collection, cur contact.Card, revs []store.Revision) []photoVersion {
	curInfo, _ := cur.PhotoInfo()
	curKey, curW := curInfo.Key, max(curInfo.Width, curInfo.Height)

	var out []photoVersion

	seen := map[string]int{}

	for _, rev := range revs {
		if rev.ETag == "" {
			continue
		}

		card, _, err := h.parse(book, rev.ETag)
		if err != nil || card == nil {
			continue
		}

		p, ok := card.PhotoInfo()
		if !ok {
			continue
		}

		key := p.Key

		if i, ok := seen[key]; ok {
			// An older revision with the same photo: it has had it since then.
			out[i].Since, out[i].ETag, out[i].Device = rev.When, rev.ETag, Device(rev.Requester)

			continue
		}

		seen[key] = len(out)
		out = append(out, photoVersion{
			ETag: rev.ETag, Since: rev.When, Device: Device(rev.Requester), Width: p.Width, Height: p.Height,
			Current: key == curKey, Better: max(p.Width, p.Height) > curW,
		})
	}

	return out
}

// sameOrigin is the CSRF check for the UI's writes: the browser's Origin
// (or Referer) must name this host. The session cookie is SameSite=Lax as
// well, so a cross-site POST carries no session in the first place.
func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}

	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return false
	}

	// Behind a proxy that rewrites Host, the browser's host is forwarded.
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" && u.Host == fwd {
		return true
	}

	return u.Host == r.Host
}

// restorePhoto puts the photo of an earlier revision back on a card.
func (h *Handler) restorePhoto(w http.ResponseWriter, r *http.Request) {
	bookName, name := r.PathValue("book"), r.PathValue("name")
	back := h.prefix + "/contacts/" + url.PathEscape(bookName) + "/" + url.PathEscape(name)

	if h.write == nil || !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	fail := func(msg string) {
		http.Redirect(w, r, back+"?error="+url.QueryEscape(msg)+"#photos", http.StatusSeeOther)
	}

	book, err := h.store.Collection(store.AddressBook, bookName)
	if err != nil {
		http.NotFound(w, r)

		return
	}

	snap, err := book.Snapshot()
	if err != nil {
		fail(err.Error())

		return
	}

	obj, ok := snap.Lookup(name)
	if !ok {
		http.NotFound(w, r)

		return
	}

	if r.PostFormValue("if_match") != obj.ETag {
		fail("the card changed since the page was loaded; look again and retry")

		return
	}

	from := r.PostFormValue("from")

	if !h.inHistory(book, name, from) {
		fail("that photo is not in this card's history")

		return
	}

	cur, _, err1 := h.full(book, obj.ETag)
	old, _, err2 := h.full(book, from)

	if err1 != nil || err2 != nil || cur == nil || old == nil {
		fail("could not read the card")

		return
	}

	data := contact.WithPhotoOf(*cur, *old).Encode()
	user := userFrom(r.Context())

	if _, err := h.write(bookName, name, data, obj.ETag, "farvardin web UI ("+user+")"); err != nil {
		fail(err.Error())

		return
	}

	h.log.Info("photo restored from history", "card", name, "from", from, "user", user)
	http.Redirect(w, r, back+"?restored=1#photos", http.StatusSeeOther)
}

// fixPhones writes the card's numbers without a country code in E.164,
// the fixes lint suggests, through the validated write path.
func (h *Handler) fixPhones(w http.ResponseWriter, r *http.Request) {
	bookName, name := r.PathValue("book"), r.PathValue("name")
	back := h.prefix + "/contacts/" + url.PathEscape(bookName) + "/" + url.PathEscape(name)

	if h.write == nil || !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	fail := func(msg string) {
		http.Redirect(w, r, back+"?error="+url.QueryEscape(msg), http.StatusSeeOther)
	}

	book, err := h.store.Collection(store.AddressBook, bookName)
	if err != nil {
		http.NotFound(w, r)

		return
	}

	snap, err := book.Snapshot()
	if err != nil {
		fail(err.Error())

		return
	}

	obj, ok := snap.Lookup(name)
	if !ok {
		http.NotFound(w, r)

		return
	}

	if r.PostFormValue("if_match") != obj.ETag {
		fail("the card changed since the page was loaded; look again and retry")

		return
	}

	cur, _, err := h.full(book, obj.ETag)
	if err != nil || cur == nil {
		fail("could not read the card")

		return
	}

	fixed, fixes := contact.FixPhones(*cur, h.region)
	if len(fixes) == 0 {
		http.Redirect(w, r, back, http.StatusSeeOther)

		return
	}

	user := userFrom(r.Context())

	if _, err := h.write(bookName, name, fixed.Encode(), obj.ETag, "farvardin web UI ("+user+")"); err != nil {
		fail(err.Error())

		return
	}

	h.log.Info("phone numbers fixed", "card", name, "count", len(fixes), "user", user)
	http.Redirect(w, r, back+"?phones="+strconv.Itoa(len(fixes)), http.StatusSeeOther)
}

func (h *Handler) inHistory(book *store.Collection, name, etag string) bool {
	revs, err := book.History(name, 200)
	if err != nil || etag == "" {
		return false
	}

	return slices.ContainsFunc(revs, func(r store.Revision) bool { return r.ETag == etag })
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

	// ?rev=<blob id> shows the photo of an earlier revision of this card.
	if rev := r.URL.Query().Get("rev"); rev != "" {
		if !h.inHistory(book, r.PathValue("name"), rev) {
			http.NotFound(w, r)

			return
		}

		obj.ETag = rev
	}

	size, _ := strconv.Atoi(r.URL.Query().Get("s"))
	etag := `"` + obj.ETag + "-" + strconv.Itoa(size) + `"`

	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)

		return
	}

	data, media, ok := h.thumbnail(book, obj, size)
	if !ok {
		http.NotFound(w, r)

		return
	}

	// A card may carry any media type in a data: URI; only images are
	// served as what they claim, so a crafted PHOTO cannot become HTML on
	// this origin.
	if !strings.HasPrefix(media, "image/") || strings.Contains(media, "svg") {
		media = "application/octet-stream"
	}

	w.Header().Set("Content-Type", media)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("ETag", etag)

	// A URL that names the blob (?v= or ?rev=) never changes meaning.
	if r.URL.Query().Get("v") == obj.ETag || r.URL.Query().Get("rev") != "" {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=3600")
	}

	_, _ = w.Write(data) //nolint:gosec // an image (or octet-stream) with nosniff and a sandbox CSP, never HTML
}

// thumbnail returns a card's photo scaled to size, from the cache when it
// was made before.
func (h *Handler) thumbnail(book *store.Collection, obj store.Object, size int) ([]byte, string, bool) {
	key := obj.ETag + "-" + strconv.Itoa(size)
	if t, ok := h.thumbs.get(key); ok {
		return t.data, t.media, true
	}

	card, _, err := h.full(book, obj.ETag)
	if err != nil || card == nil {
		return nil, "", false
	}

	p, err := card.Photo()
	if err != nil {
		return nil, "", false
	}

	data, media, err := p.Thumbnail(size)
	if err != nil {
		// Not decodable (or too large to decode): serve it as it is, but
		// do not let an original crowd out the thumbnails in the cache.
		return p.Data, p.MediaType, true
	}

	if size > 0 {
		h.thumbs.put(key, thumb{data: data, media: media})
	}

	return data, media, true
}

func truncate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// upcoming is a birthday or anniversary on the dashboard.
type upcoming struct {
	When     time.Time
	Days     int
	Name     string
	What     string
	Age      int
	Link     string
	Book     string
	File     string
	ETag     string
	HasPhoto bool
}

// eventRow is one event in an agenda.
type eventRow struct {
	When     time.Time
	AllDay   bool
	Title    string
	Location string
	Repeats  bool
	Kind     string
	Calendar string
}

// dayGroup is one day of an agenda.
type dayGroup struct {
	Label string
	Date  time.Time
	Today bool
	Rows  []eventRow
}

// dashboardData collects the tiles and the birthdays of the next 30 days.
func (h *Handler) dashboardData(now time.Time) (map[string]int, []upcoming) {
	today := truncate(now)
	stats := map[string]int{"contacts": 0, "photos": 0, "lowres": 0, "errors": 0, "week": 0}

	var soon []upcoming

	for _, book := range h.store.Collections(store.AddressBook) {
		people, snap, err := h.people(book)
		if err != nil {
			continue
		}

		stats["contacts"] += len(people)

		for _, p := range people {
			if p.HasPhoto {
				stats["photos"]++
			}

			if p.LowRes {
				stats["lowres"]++
			}

			if p.Issues > 0 {
				stats["errors"]++
			}

			obj, _ := snap.Lookup(p.File)

			card, _, err := h.parse(book, obj.ETag)
			if err != nil || card == nil {
				continue
			}

			for _, d := range card.Dates() {
				if d.Kind == contact.OtherDate {
					continue
				}

				next := d.Next(now)

				days := int(next.Sub(today).Hours() / 24)
				if days > 30 {
					continue
				}

				u := upcoming{
					When: next, Days: days, Name: p.Name, What: string(d.Kind), Age: 0,
					Link: h.prefix + "/contacts/" + book.Name() + "/" + p.File,
					Book: book.Name(), File: p.File, ETag: p.ETag, HasPhoto: p.HasPhoto,
				}

				if d.Year > 0 {
					u.Age = next.Year() - d.Year
				}

				soon = append(soon, u)
			}
		}
	}

	sort.Slice(soon, func(i, j int) bool { return soon[i].When.Before(soon[j].When) })

	return stats, soon
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	today := truncate(now)
	stats, soon := h.dashboardData(now)

	var recent []map[string]any

	devices := map[string]int{}

	for _, kind := range []store.Kind{store.AddressBook, store.Calendar} {
		for _, c := range h.store.Collections(kind) {
			revs, _ := c.Recent(200)

			for i, rev := range revs {
				if now.Sub(rev.When) <= 30*24*time.Hour {
					devices[Device(rev.Requester)]++
				}

				if i < 10 {
					title, link := h.subject(c, kind, rev)
					recent = append(recent, map[string]any{
						"Rev": rev, "Collection": c.Name(), "Kind": string(kind), "Device": Device(rev.Requester),
						"Title": title, "Link": link,
					})
				}
			}
		}
	}

	sort.Slice(recent, func(i, j int) bool {
		a, _ := recent[i]["Rev"].(store.Revision)
		b, _ := recent[j]["Rev"].(store.Revision)

		return a.When.After(b.When)
	})

	type deviceCount struct {
		Name  string
		Count int
	}

	deviceList := make([]deviceCount, 0, len(devices))
	maxDevice := 0

	for name, n := range devices {
		deviceList = append(deviceList, deviceCount{Name: name, Count: n})
		maxDevice = max(maxDevice, n)
	}

	sort.Slice(deviceList, func(i, j int) bool { return deviceList[i].Count > deviceList[j].Count })

	week := h.agenda(today, today.AddDate(0, 0, 7))
	for _, g := range week {
		stats["week"] += len(g.Rows)
	}

	h.render(w, r, "dashboard.html", map[string]any{
		"Title": "Farvardin", "Stats": stats, "Soon": soon, "Recent": recent[:min(12, len(recent))],
		"Devices": deviceList, "MaxDevice": maxDevice, "Week": week, "Greeting": greeting(now),
	})
}

// subject names what a commit touched ("Edited Alice Example") from its
// "Added|Modified|Deleted <file>" message, with a link while it exists.
func (h *Handler) subject(c *store.Collection, kind store.Kind, rev store.Revision) (string, string) {
	verb, file, ok := strings.Cut(rev.Message, " ")
	if !ok {
		return rev.Message, ""
	}

	word := map[string]string{"Added": "Added", "Modified": "Edited", "Deleted": "Deleted"}[verb]
	if word == "" {
		return rev.Message, ""
	}

	etag := rev.ETag

	snap, err := c.Snapshot()
	if err == nil {
		if obj, ok := snap.Lookup(file); ok {
			etag = obj.ETag
		}
	}

	if etag == "" {
		return word + " " + file, ""
	}

	card, cal, err := h.parse(c, etag)

	switch {
	case err != nil:
		return word + " " + file, ""
	case card != nil && kind == store.AddressBook:
		link := ""
		if word != "Deleted" {
			link = h.prefix + "/contacts/" + c.Name() + "/" + file
		}

		return word + " " + card.DisplayName(), link
	case cal != nil:
		if s, ok := calendar.Summarize(cal); ok && s.Title != "" {
			return word + " " + s.Title, h.prefix + "/calendars/" + c.Name() + "/"
		}
	}

	return word + " " + file, ""
}

func greeting(t time.Time) string {
	switch h := t.Hour(); {
	case h < 5:
		return "Good night"
	case h < 12:
		return "Good morning"
	case h < 18:
		return "Good afternoon"
	default:
		return "Good evening"
	}
}

// agenda collects events of every calendar (the birthdays one included)
// between from and to, grouped by day.
func (h *Handler) agenda(from, to time.Time) []dayGroup {
	var rows []eventRow

	add := func(calName string, cal *calendar.Component) {
		if cal == nil {
			return
		}

		s, ok := calendar.Summarize(cal)
		if !ok {
			return
		}

		next, _ := calendar.NextOccurrence(cal, from)
		if next.Before(from) || !next.Before(to) {
			return
		}

		rows = append(rows, eventRow{When: next, AllDay: s.AllDay, Title: s.Title, Location: s.Location, Repeats: s.Repeats, Kind: s.Kind, Calendar: calName})
	}

	for _, c := range h.store.Collections(store.Calendar) {
		snap, err := c.Snapshot()
		if err != nil {
			continue
		}

		for _, o := range snap.Objects {
			_, cal, _ := h.parse(c, o.ETag)
			add(c.Name(), cal)
		}
	}

	if snap, read := h.birthdays(); snap != nil {
		for _, o := range snap.Objects {
			if data, err := read(o.ETag); err == nil {
				cal, _ := calendar.Parse(data)
				add("birthdays", cal)
			}
		}
	}

	return groupRows(rows, from)
}

// groupRows sorts events and groups them by local day.
func groupRows(rows []eventRow, today time.Time) []dayGroup {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].AllDay != rows[j].AllDay && truncate(rows[i].When).Equal(truncate(rows[j].When)) {
			return rows[i].AllDay
		}

		return rows[i].When.Before(rows[j].When)
	})

	var out []dayGroup

	for _, row := range rows {
		d := truncate(row.When.Local()) //nolint:gosmopolitan // the agenda is in the server's local time

		if len(out) == 0 || !out[len(out)-1].Date.Equal(d) {
			label := d.Format("Monday 2 January")

			switch days := int(d.Sub(truncate(today)).Hours() / 24); days {
			case 0:
				label = "Today"
			case 1:
				label = "Tomorrow"
			}

			out = append(out, dayGroup{Label: label, Date: d, Today: d.Equal(truncate(today)), Rows: nil})
		}

		out[len(out)-1].Rows = append(out[len(out)-1].Rows, row)
	}

	return out
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("cal")
	today := truncate(time.Now())
	horizon := today.AddDate(0, 0, 90)
	title := name

	var rows []eventRow

	collect := func(cal *calendar.Component) {
		if cal == nil {
			return
		}

		s, ok := calendar.Summarize(cal)
		if !ok {
			return
		}

		next, _ := calendar.NextOccurrence(cal, today)
		if next.Before(today) || next.After(horizon) {
			return
		}

		rows = append(rows, eventRow{When: next, AllDay: s.AllDay, Title: s.Title, Location: s.Location, Repeats: s.Repeats, Kind: s.Kind, Calendar: name})
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

		for _, o := range snap.Objects {
			_, cal, _ := h.parse(col, o.ETag)
			collect(cal)
		}
	} else if snap, read := h.birthdays(); name == "birthdays" && snap != nil {
		title = "Birthdays"

		for _, o := range snap.Objects {
			if data, err := read(o.ETag); err == nil {
				cal, _ := calendar.Parse(data)
				collect(cal)
			}
		}
	} else {
		http.NotFound(w, r)

		return
	}

	h.render(w, r, "events.html", map[string]any{
		"Title": title, "Days": groupRows(rows, today), "Count": len(rows),
		"Feed": h.davPrefix + "/" + h.store.Principal() + "/calendars/" + name + "/",
	})
}
