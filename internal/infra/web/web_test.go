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

	ui, err := web.New(s, d.Cache(), d.Birthdays, web.Options{Prefix: "/ui", DAVPrefix: "", Sessions: sessions, Writer: d.WriteCard, Full: d.Full(), ThumbnailCacheBytes: 1 << 20}, log)
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
