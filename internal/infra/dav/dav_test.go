package dav_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/dav"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	h   *dav.Handler
}

func newClient(t *testing.T) *client {
	t.Helper()

	s, err := store.Open(t.TempDir(), "user", store.Author{Name: "T", Email: "t@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.EnsureDefaults(); err != nil {
		t.Fatal(err)
	}

	h := dav.New(s, dav.Options{
		Prefix: "", UIPath: "/ui/", MaxResourceSize: 1 << 20,
		Photo:     contact.PhotoPolicy{MaxSize: 720, Guard: true, Stabilize: true, ThumbnailSize: 128, Quality: 85},
		Birthdays: true, BirthdayAlarmDays: 0, BirthdayColor: "#FF2D55", AllowCollectionDelete: false, CalendarUserAddress: "",
	}, slog.New(slog.DiscardHandler), nil)

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return &client{t: t, srv: srv, h: h}
}

func (c *client) do(method, path, body string, headers ...string) (int, http.Header, string) {
	c.t.Helper()

	req, err := http.NewRequestWithContext(c.t.Context(), method, c.srv.URL+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}

	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, resp.Header, string(data)
}

func expect(t *testing.T, what string, got, want int, body string) {
	t.Helper()

	if got != want {
		t.Fatalf("%s: status %d, want %d\n%s", what, got, want, body)
	}
}

func mustContain(t *testing.T, what, body string, parts ...string) {
	t.Helper()

	for _, p := range parts {
		if !strings.Contains(body, p) {
			t.Fatalf("%s: missing %q in\n%s", what, p, body)
		}
	}
}

const (
	alice = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:alice\r\nFN:Alice Example\r\nN:Example;Alice;;;\r\nEMAIL;TYPE=INTERNET;TYPE=HOME;TYPE=pref:alice@example.com\r\nBDAY:1990-03-16\r\nEND:VCARD\r\n"
	bob   = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:bob\r\nFN:Bob\r\nN:;Bob;;;\r\nEND:VCARD\r\n"
	event = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:test\r\nBEGIN:VEVENT\r\nUID:ev1\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20261110T100000Z\r\nDTEND:20261110T110000Z\r\nSUMMARY:Meeting\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
)

var syncTokenRe = regexp.MustCompile(`<d:sync-token>([^<]+)</d:sync-token>`)

func TestDiscovery(t *testing.T) {
	t.Parallel()

	c := newClient(t)

	st, _, body := c.do("PROPFIND", "/", `<d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`, "Depth", "0")
	expect(t, "root", st, http.StatusMultiStatus, body)
	mustContain(t, "root", body, "<d:href>/user/</d:href>")

	st, _, body = c.do("PROPFIND", "/user/", `<d:propfind xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav" xmlns:cal="urn:ietf:params:xml:ns:caldav"><d:prop><card:addressbook-home-set/><cal:calendar-home-set/></d:prop></d:propfind>`, "Depth", "0")
	expect(t, "principal", st, http.StatusMultiStatus, body)
	mustContain(t, "principal", body, "/user/contacts/", "/user/calendars/")

	st, _, body = c.do("PROPFIND", "/user/calendars/", `<d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:displayname/></d:prop></d:propfind>`, "Depth", "1")
	expect(t, "calendar home", st, http.StatusMultiStatus, body)
	mustContain(t, "calendar home", body, "/user/calendars/calendar/", "/user/calendars/birthdays/", "<cal:calendar/>", "Birthdays")

	st, hdr, _ := c.do("GET", "/.well-known/carddav", "")
	if st != http.StatusOK && hdr.Get("Location") == "" {
		t.Fatalf("well-known: %d", st)
	}
}

func TestCardLifecycleAndSync(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	book := "/user/contacts/addressbook/"

	st, _, body := c.do("PROPFIND", book, `<d:propfind xmlns:d="DAV:"><d:prop><d:sync-token/></d:prop></d:propfind>`, "Depth", "0")
	expect(t, "token", st, http.StatusMultiStatus, body)

	start := syncTokenRe.FindStringSubmatch(body)[1]

	st, hdr, body := c.do("PUT", book+"alice.vcf", alice, "If-None-Match", "*", "Content-Type", "text/vcard")
	expect(t, "put alice", st, http.StatusCreated, body)

	etag := hdr.Get("ETag")
	if etag == "" {
		t.Fatal("PUT of an unchanged card must return an ETag")
	}

	st, _, body = c.do("PUT", book+"alice.vcf", alice, "If-None-Match", "*")
	expect(t, "put again with If-None-Match", st, http.StatusPreconditionFailed, body)

	st, _, body = c.do("PUT", book+"other.vcf", alice)
	expect(t, "uid conflict", st, http.StatusForbidden, body)
	mustContain(t, "uid conflict", body, "no-uid-conflict")

	st, _, body = c.do("PUT", book+"bad.vcf", "BEGIN:VCARD\r\nVERSION:3.0\r\nEND:VCARD\r\n")
	expect(t, "invalid card", st, http.StatusForbidden, body)
	mustContain(t, "invalid card", body, "valid-address-data", "UID is missing")

	st, _, body = c.do("PUT", book+"bob.vcf", bob)
	expect(t, "put bob", st, http.StatusCreated, body)

	syncBody := `<d:sync-collection xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:sync-token>` + start + `</d:sync-token><d:sync-level>1</d:sync-level><d:prop><d:getetag/></d:prop></d:sync-collection>`
	st, _, body = c.do("REPORT", book, syncBody)
	expect(t, "sync", st, http.StatusMultiStatus, body)
	mustContain(t, "sync", body, "alice.vcf", "bob.vcf", etag)

	mid := syncTokenRe.FindStringSubmatch(body)[1]

	st, _, body = c.do("DELETE", book+"bob.vcf", "", "If-Match", `"nope"`)
	expect(t, "delete with stale If-Match", st, http.StatusPreconditionFailed, body)

	st, _, body = c.do("DELETE", book+"bob.vcf", "")
	expect(t, "delete", st, http.StatusNoContent, body)

	st, _, body = c.do("REPORT", book, strings.Replace(syncBody, start, mid, 1))
	expect(t, "sync after delete", st, http.StatusMultiStatus, body)
	mustContain(t, "sync after delete", body, "bob.vcf", "404 Not Found")

	st, _, body = c.do("REPORT", book, strings.Replace(syncBody, start, "0123456789abcdef0123456789abcdef01234567", 1))
	expect(t, "bad token", st, http.StatusForbidden, body)
	mustContain(t, "bad token", body, "valid-sync-token")

	multiget := `<card:addressbook-multiget xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:prop><d:getetag/><card:address-data content-type="text/vcard" version="4.0"/></d:prop><d:href>` + book + `alice.vcf</d:href><d:href>` + book + `missing.vcf</d:href></card:addressbook-multiget>`
	st, _, body = c.do("REPORT", book, multiget)
	expect(t, "multiget", st, http.StatusMultiStatus, body)
	mustContain(t, "multiget", body, "VERSION:4.0", "EMAIL;PREF=1;TYPE=home:alice@example.com", "missing.vcf")

	query := `<card:addressbook-query xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:prop><d:getetag/></d:prop><card:filter><card:prop-filter name="FN"><card:text-match collation="i;unicode-casemap" match-type="contains">ALICE</card:text-match></card:prop-filter></card:filter></card:addressbook-query>`
	st, _, body = c.do("REPORT", book, query)
	expect(t, "query", st, http.StatusMultiStatus, body)
	mustContain(t, "query", body, "alice.vcf")

	st, hdr, body = c.do("GET", book+"alice.vcf", "", "Accept", "application/jscontact+json")
	expect(t, "jscontact", st, http.StatusOK, body)
	mustContain(t, "jscontact", body, `"@type": "Card"`, `"address": "alice@example.com"`)

	if !strings.Contains(hdr.Get("Content-Type"), "jscontact") {
		t.Fatalf("content type %q", hdr.Get("Content-Type"))
	}

	// The birthdays calendar follows the address book.
	st, _, body = c.do("PROPFIND", "/user/calendars/birthdays/", `<d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>`, "Depth", "1")
	expect(t, "birthdays", st, http.StatusMultiStatus, body)
	mustContain(t, "birthdays", body, "alice-birthday.ics")

	st, _, body = c.do("GET", "/user/calendars/birthdays/alice-birthday.ics", "")
	expect(t, "birthday event", st, http.StatusOK, body)
	mustContain(t, "birthday event", body, "RRULE:FREQ=YEARLY", "DTSTART;VALUE=DATE:19900316", "Alice Example (1990)")

	st, _, body = c.do("PUT", "/user/calendars/birthdays/x.ics", event)
	expect(t, "birthdays are read-only", st, http.StatusForbidden, body)
}

func TestCalendarQueryAndProppatch(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	cal := "/user/calendars/calendar/"

	st, _, body := c.do("PUT", cal+"ev1.ics", event)
	expect(t, "put event", st, http.StatusCreated, body)

	query := func(start, end string) string {
		return `<cal:calendar-query xmlns:d="DAV:" xmlns:cal="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/></d:prop><cal:filter><cal:comp-filter name="VCALENDAR"><cal:comp-filter name="VEVENT"><cal:time-range start="` + start + `" end="` + end + `"/></cal:comp-filter></cal:comp-filter></cal:filter></cal:calendar-query>`
	}

	st, _, body = c.do("REPORT", cal, query("20261101T000000Z", "20261201T000000Z"))
	expect(t, "query in range", st, http.StatusMultiStatus, body)
	mustContain(t, "query in range", body, "ev1.ics")

	_, _, body = c.do("REPORT", cal, query("20270101T000000Z", "20270201T000000Z"))
	if strings.Contains(body, "ev1.ics") {
		t.Fatalf("event outside the range was returned:\n%s", body)
	}

	st, _, body = c.do("PROPPATCH", cal, `<d:propertyupdate xmlns:d="DAV:" xmlns:ical="http://apple.com/ns/ical/"><d:set><d:prop><d:displayname>Home</d:displayname><ical:calendar-color>#00FF00</ical:calendar-color></d:prop></d:set></d:propertyupdate>`)
	expect(t, "proppatch", st, http.StatusMultiStatus, body)
	mustContain(t, "proppatch", body, "200 OK")

	_, _, body = c.do("PROPFIND", cal, `<d:propfind xmlns:d="DAV:" xmlns:ical="http://apple.com/ns/ical/"><d:prop><d:displayname/><ical:calendar-color/></d:prop></d:propfind>`, "Depth", "0")
	mustContain(t, "after proppatch", body, "Home", "#00FF00")

	st, _, body = c.do("MKCALENDAR", "/user/calendars/work/", "")
	expect(t, "mkcalendar", st, http.StatusCreated, body)

	_, _, body = c.do("GET", cal, "")
	mustContain(t, "webcal export", body, "BEGIN:VCALENDAR", "SUMMARY:Meeting", "X-WR-CALNAME:Home")
}

func TestHrefsAreEscapedLikeXandikos(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	cal := "/user/calendars/calendar/"
	ev := strings.Replace(event, "UID:ev1", "UID:x@google.com", 1)

	st, _, body := c.do("PUT", cal+"x%40google.com.ics", ev)
	expect(t, "put", st, http.StatusCreated, body)

	_, _, body = c.do("PROPFIND", cal, `<d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>`, "Depth", "1")
	mustContain(t, "escaped href", body, "<d:href>/user/calendars/calendar/x%40google.com.ics</d:href>")

	st, _, body = c.do("GET", cal+"x@google.com.ics", "")
	expect(t, "unescaped request path", st, http.StatusOK, body)
}

// A write from the web UI or MCP skips the photo policy, but a photo
// written the vCard 3.0 way inside a 4.0 card is still rewritten as a
// data: URI, which is the only form DAVx5 reads in 4.0.
func TestWriteCardFitsPhotoToVersion(t *testing.T) {
	t.Parallel()

	c := newClient(t)

	// A 1x1 GIF is enough: the line form is what is checked.
	const gif = "R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"

	broken := "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:mixed\r\nFN:Mixed\r\n" +
		"PHOTO;ENCODING=b;TYPE=GIF:" + gif + "\r\nEND:VCARD\r\n"

	if _, err := c.h.WriteCard("addressbook", "mixed.vcf", []byte(broken), "", "test"); err != nil {
		t.Fatal(err)
	}

	st, _, body := c.do("GET", "/user/contacts/addressbook/mixed.vcf", "")
	expect(t, "get", st, http.StatusOK, body)

	body = strings.ReplaceAll(body, "\r\n ", "") // unfold
	mustContain(t, "fitted photo", body, "PHOTO:data:image/gif;base64,"+gif)

	if strings.Contains(body, "ENCODING=") {
		t.Fatalf("4.0 card still has ENCODING:\n%s", body)
	}
}
