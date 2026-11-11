package web_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/auth"
	"github.com/1995parham-me/farvardin/internal/infra/dav"
	"github.com/1995parham-me/farvardin/internal/infra/store"
	"github.com/1995parham-me/farvardin/internal/infra/web"
)

func jpegB64(t *testing.T, size int) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func card(photo string) string {
	bday := time.Now().AddDate(0, 0, 10).Format("--0102")

	return "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:a\r\nFN:Alice Example\r\nN:Example;Alice;;;\r\nTEL;TYPE=cell:+15555550100\r\nBDAY:" + bday + "\r\nPHOTO;ENCODING=b;TYPE=JPEG:" + photo + "\r\nEND:VCARD\r\n"
}

func setup(t *testing.T) (*httptest.Server, *http.Client, *store.Collection) {
	t.Helper()

	return setupRegion(t)
}

func setupRegion(t *testing.T, region ...string) (*httptest.Server, *http.Client, *store.Collection) {
	t.Helper()

	root := t.TempDir()

	s, err := store.Open(root, "user", store.Author{Name: "T", Email: "t@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.EnsureDefaults(); err != nil {
		t.Fatal(err)
	}

	book, _ := s.Collection(store.AddressBook, "addressbook")

	// History: a big photo from the iPhone, then a thumbnail from DAVx5.
	if _, _, err := book.Put("a.vcf", []byte(card(jpegB64(t, 400))), store.Condition{}, "iOS/27.0.1 (24A446) dataaccessd/1.0"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := book.Put("a.vcf", []byte(card(jpegB64(t, 96))), store.Condition{}, "DAVx5/4.5.10-ose (at.bitfire.davdroid)"); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.DiscardHandler)
	d := dav.New(s, dav.Options{
		Prefix: "", UIPath: "/ui/", MaxResourceSize: 1 << 20,
		Photo:     contact.PhotoPolicy{MaxSize: 0, Guard: false, Stabilize: false, ThumbnailSize: 0, Quality: 0},
		Birthdays: true, BirthdayAlarmDays: 0, BirthdayColor: "#FF2D55", AllowCollectionDelete: false, CalendarUserAddress: "", Push: nil,
	}, log, nil)

	hash, err := auth.Hash("s3cret")
	if err != nil {
		t.Fatal(err)
	}

	sessions, err := web.NewSessions(web.SessionOptions{
		Users: map[string]string{"parham": hash}, SecretFile: filepath.Join(root, "ui.key"), TTL: 0, Secure: false, CookiePath: "/ui",
	})
	if err != nil {
		t.Fatal(err)
	}

	ui, err := web.New(s, d.Cache(), d.Birthdays, web.Options{Prefix: "/ui", DAVPrefix: "", Sessions: sessions, Writer: d.WriteCard, Full: d.Full(), ThumbnailCacheBytes: 1 << 20, PhoneRegion: region, Deleter: d.DeleteObject}, log)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/ui/", ui)
	mux.Handle("/", d)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar} //nolint:exhaustruct // test client

	return srv, client, book
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()

	resp, err := c.Get(u) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	b, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(b)
}

func TestLoginPagesAndPhotoRestore(t *testing.T) {
	t.Parallel()

	srv, c, book := setup(t)

	// Not signed in: every page sends you to the login page.
	st, body := get(t, c, srv.URL+"/ui/")
	if st != http.StatusOK || !strings.Contains(body, `name="password"`) {
		t.Fatalf("expected the login page, got %d", st)
	}

	resp, err := c.PostForm(srv.URL+"/ui/login", url.Values{"username": {"parham"}, "password": {"wrong"}, "next": {"/ui/"}}) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}

	resp, err = c.PostForm(srv.URL+"/ui/login", url.Values{"username": {"parham"}, "password": {"s3cret"}, "next": {"//evil.example/"}}) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Host != strings.TrimPrefix(srv.URL, "http://") || resp.Request.URL.Path != "/ui/" {
		t.Fatalf("login: %d at %s", resp.StatusCode, resp.Request.URL)
	}

	pages := map[string][]string{
		"/ui/":                                {"Good ", "Low-res photos", "Android · DAVx5 4.5.10", "Coming up"},
		"/ui/contacts/addressbook/":           {"Alice Example", "Low-res photo", "96px"},
		"/ui/contacts/addressbook/?f=lowres":  {"Alice Example"},
		"/ui/contacts/addressbook/a.vcf":      {"Photo history", "400×400", "96×96", "photo 400×400 → 96×96", "iPhone · iOS 27.0.1", "Restore", "data-copy"},
		"/ui/calendars/birthdays/":            {"Alice Example"},
		"/ui/calendars/calendar/":             {"Nothing in the next 90 days"},
		"/ui/lint":                            {"Lint"},
		"/ui/static/style.css":                {"--pink: #FF69B4"},
		"/ui/photo/addressbook/a.vcf?s=96&v=": {"\xff\xd8"},
	}

	for page, wants := range pages {
		st, body := get(t, c, srv.URL+page)
		if st != http.StatusOK {
			t.Fatalf("%s: %d\n%s", page, st, body)
		}

		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Fatalf("%s lacks %q", page, w)
			}
		}
	}

	// Restore the 400 px photo. Without a same-origin Origin it is refused.
	revs, _ := book.History("a.vcf", 10)
	snap, _ := book.Snapshot()
	cur, _ := snap.Lookup("a.vcf")
	form := url.Values{"from": {revs[1].ETag}, "if_match": {cur.ETag}}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/ui/contacts/addressbook/a.vcf/photo", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("restore without Origin: %d", resp.StatusCode)
	}

	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/ui/contacts/addressbook/a.vcf/photo", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)

	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Request.URL.RawQuery, "restored=1") {
		t.Fatalf("restore: %d at %s", resp.StatusCode, resp.Request.URL)
	}

	data, _, _ := book.Read("a.vcf")

	restored, err := contact.Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	if p, _ := restored.Photo(); p.Width != 400 {
		t.Fatalf("restored photo is %d px", p.Width)
	}

	if after, _ := book.History("a.vcf", 1); !strings.HasPrefix(after[0].Requester, "farvardin web UI (parham)") {
		t.Fatalf("requester %q", after[0].Requester)
	}

	// Sign out ends the session.
	resp, err = c.PostForm(srv.URL+"/ui/logout", nil) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if _, body := get(t, c, srv.URL+"/ui/"); !strings.Contains(body, `name="password"`) {
		t.Fatal("still signed in after logout")
	}
}

func TestPhoneLintAndFix(t *testing.T) {
	t.Parallel()

	srv, c, book := setupRegion(t, "IR")

	const vcard = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:b\r\nFN:Babak\r\nN:;Babak;;;\r\n" +
		"item1.TEL;type=CELL:0912 345 6789\r\nitem1.X-ABLabel:Persian\r\nEND:VCARD\r\n"

	if _, _, err := book.Put("b.vcf", []byte(vcard), store.Condition{}, "iOS/27.0.1 (24A446) dataaccessd/1.0"); err != nil {
		t.Fatal(err)
	}

	resp, err := c.PostForm(srv.URL+"/ui/login", url.Values{"username": {"parham"}, "password": {"s3cret"}, "next": {"/ui/"}}) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	pages := map[string][]string{
		"/ui/lint":                                  {"is not in international form → &#43;989123456789"},
		"/ui/contacts/addressbook/b.vcf":            {"Add country codes (1)", `href="tel:&#43;989123456789"`},
		"/ui/contacts/addressbook/?q=%2B98912345":   {"Babak"},
		"/ui/contacts/addressbook/?q=0912+345+6789": {"Babak"},
	}

	for page, wants := range pages {
		_, body := get(t, c, srv.URL+page)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q:\n%s", page, want, body)
			}
		}
	}

	snap, _ := book.Snapshot()
	cur, _ := snap.Lookup("b.vcf")

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/ui/contacts/addressbook/b.vcf/phones", strings.NewReader(url.Values{"if_match": {cur.ETag}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)

	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Request.URL.RawQuery, "phones=1") {
		t.Fatalf("fix: %d at %s", resp.StatusCode, resp.Request.URL)
	}

	data, _, _ := book.Read("b.vcf")
	if want := strings.Replace(vcard, "0912 345 6789", "+989123456789", 1); string(data) != want {
		t.Fatalf("stored:\n%s\nwant:\n%s", data, want)
	}

	if _, body := get(t, c, srv.URL+"/ui/lint"); strings.Contains(body, "international form") {
		t.Fatal("still reported after the fix")
	}
}

// post sends a form from the UI's own origin and returns where the
// redirects ended.
func post(t *testing.T, c *http.Client, srv *httptest.Server, path string, form url.Values) *url.URL {
	t.Helper()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)

	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	return resp.Request.URL
}

func TestTidy(t *testing.T) {
	t.Parallel()

	srv, c, book := setupRegion(t, "IR", "ES")

	putCard := func(name, uid, fn string, lines ...string) string {
		t.Helper()

		data := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:" + uid + "\r\nFN:" + fn + "\r\nN:;" + fn + ";;;\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VCARD\r\n"

		etag, _, err := book.Put(name, []byte(data), store.Condition{}, "DAVx5/4.5.10-ose (at.bitfire.davdroid)")
		if err != nil {
			t.Fatal(err)
		}

		return etag
	}

	ka := putCard("k.vcf", "k", "Kubit", "TEL:0912 352 5311")
	kb := putCard("l.vcf", "l", "Kubit Support", "TEL:+989123525311", "EMAIL:help@kubit.example")
	putCard("m.vcf", "m", "Correos", "TEL:900 400 004")

	resp, err := c.PostForm(srv.URL+"/ui/login", url.Values{"username": {"parham"}, "password": {"s3cret"}, "next": {"/ui/"}}) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	// Duplicates, then merge l.vcf into k.vcf.
	if _, body := get(t, c, srv.URL+"/ui/duplicates"); !strings.Contains(body, "same number &#43;989123525311") || !strings.Contains(body, "Kubit Support") {
		t.Fatalf("duplicates page:\n%s", body)
	}

	if u := post(t, c, srv, "/ui/duplicates/merge", url.Values{"book": {"addressbook"}, "keep": {"k.vcf"}, "id": {"k.vcf", "l.vcf"}, "etag": {ka, "stale"}}); !strings.Contains(u.RawQuery, "error=") {
		t.Fatalf("stale merge went through: %s", u)
	}

	u := post(t, c, srv, "/ui/duplicates/merge", url.Values{"book": {"addressbook"}, "keep": {"k.vcf"}, "id": {"k.vcf", "l.vcf"}, "etag": {ka, kb}})
	if u.Path != "/ui/contacts/addressbook/k.vcf" || u.Query().Get("merged") != "1" {
		t.Fatalf("merge: %s", u)
	}

	if data, _, _ := book.Read("k.vcf"); !strings.Contains(string(data), "EMAIL:help@kubit.example") {
		t.Fatalf("merged card:\n%s", data)
	}

	// The merged-away card is listed as deleted and can come back.
	if _, body := get(t, c, srv.URL+"/ui/deleted"); !strings.Contains(body, "Kubit Support") || !strings.Contains(body, "Web UI") {
		t.Fatalf("deleted page:\n%s", body)
	}

	u = post(t, c, srv, "/ui/deleted/restore", url.Values{"book": {"addressbook"}, "name": {"l.vcf"}})
	if u.Path != "/ui/contacts/addressbook/l.vcf" || !strings.Contains(u.RawQuery, "back=1") {
		t.Fatalf("restore deleted: %s", u)
	}

	if data, _, _ := book.Read("l.vcf"); !strings.Contains(string(data), "FN:Kubit Support") {
		t.Fatalf("restored card:\n%s", data)
	}

	// Restore k.vcf to the version before the merge.
	snap, _ := book.Snapshot()
	cur, _ := snap.Lookup("k.vcf")

	if _, body := get(t, c, srv.URL+"/ui/contacts/addressbook/k.vcf"); !strings.Contains(body, `name="from" value="`+ka+`"`) {
		t.Fatalf("no restore button for the earlier version:\n%s", body)
	}

	u = post(t, c, srv, "/ui/contacts/addressbook/k.vcf/restore", url.Values{"from": {ka}, "if_match": {cur.ETag}})
	if !strings.Contains(u.RawQuery, "version=1") {
		t.Fatalf("restore version: %s", u)
	}

	if _, etag, _ := book.Read("k.vcf"); etag != ka {
		t.Fatalf("k.vcf is %s, want the old %s", etag, ka)
	}

	// Fix every number at once: k.vcf (Iranian) and m.vcf (Spanish).
	if _, body := get(t, c, srv.URL+"/ui/lint"); !strings.Contains(body, "Add country codes everywhere (2 on 2 cards)") {
		t.Fatalf("lint page:\n%s", body)
	}

	u = post(t, c, srv, "/ui/lint/phones", nil)
	if q := u.Query(); q.Get("phones") != "2" || q.Get("cards") != "2" || q.Get("error") != "" {
		t.Fatalf("fix all: %s", u)
	}

	if data, _, _ := book.Read("m.vcf"); !strings.Contains(string(data), "TEL:+34900400004\r\n") {
		t.Fatalf("m.vcf:\n%s", data)
	}

	// k.vcf and l.vcf are a group again; mark them as different contacts.
	u = post(t, c, srv, "/ui/duplicates/notsame", url.Values{"book": {"addressbook"}, "id": {"k.vcf", "l.vcf"}})
	if u.Query().Get("notsame") != "1" {
		t.Fatalf("not the same: %s", u)
	}

	if _, body := get(t, c, srv.URL+"/ui/duplicates"); strings.Contains(body, "Kubit Support") || !strings.Contains(body, "1 group marked as different contacts.") {
		t.Fatalf("duplicates page after marking:\n%s", body)
	}

	post(t, c, srv, "/ui/duplicates/showall", nil)

	if _, body := get(t, c, srv.URL+"/ui/duplicates"); !strings.Contains(body, "Kubit Support") {
		t.Fatalf("duplicates page after show all:\n%s", body)
	}

	// Without the Origin check passing, nothing is written.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/ui/lint/phones", nil)

	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", resp.StatusCode)
	}
}

func TestGroups(t *testing.T) {
	t.Parallel()

	srv, c, book := setup(t)

	const (
		bob   = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:bob\r\nFN:Bob Builder\r\nN:Builder;Bob;;;\r\nCATEGORIES:Neighbours\r\nEND:VCARD\r\n"
		group = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:g1\r\nFN:Home Services\r\nN:Home Services;;;;\r\nX-ADDRESSBOOKSERVER-KIND:group\r\n" +
			"X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:bob\r\nX-ADDRESSBOOKSERVER-MEMBER:urn:uuid:gone\r\nEND:VCARD\r\n"
	)

	for name, data := range map[string]string{"bob.vcf": bob, "g1.vcf": group} {
		if _, _, err := book.Put(name, []byte(data), store.Condition{}, "DAVx5/4.5.10-ose (at.bitfire.davdroid)"); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := c.PostForm(srv.URL+"/ui/login", url.Values{"username": {"parham"}, "password": {"s3cret"}, "next": {"/ui/"}}) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	_, list := get(t, c, srv.URL+"/ui/contacts/addressbook/")
	if !strings.Contains(list, `href="?g=Home%20Services"`) || !strings.Contains(list, "Neighbours") || strings.Contains(list, `href="g1.vcf"`) {
		t.Fatalf("list must chip the groups and not list the group card:\n%s", list)
	}

	_, only := get(t, c, srv.URL+"/ui/contacts/addressbook/?g=Home+Services")
	if !strings.Contains(only, "Bob Builder") || strings.Contains(only, "Alice Example") {
		t.Fatalf("group filter:\n%s", only)
	}

	_, page := get(t, c, srv.URL+"/ui/contacts/addressbook/g1.vcf")
	if !strings.Contains(page, "Members") || !strings.Contains(page, `href="bob.vcf"`) || !strings.Contains(page, "1 member(s) no longer have a card") {
		t.Fatalf("group page:\n%s", page)
	}

	_, bobPage := get(t, c, srv.URL+"/ui/contacts/addressbook/bob.vcf")
	if !strings.Contains(bobPage, `href="g1.vcf"`) || !strings.Contains(bobPage, "Neighbours") {
		t.Fatalf("contact page groups:\n%s", bobPage)
	}
}
