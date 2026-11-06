package dav_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/dav"
	"github.com/1995parham-me/farvardin/internal/infra/push"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

func TestWebDAVPush(t *testing.T) {
	t.Parallel()

	delivered := make(chan http.Header, 4)
	pushSvc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered <- r.Header
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(pushSvc.Close)

	root := t.TempDir()

	s, err := store.Open(root, "user", store.Author{Name: "T", Email: "t@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.EnsureDefaults(); err != nil {
		t.Fatal(err)
	}

	keys, err := push.LoadKeys(filepath.Join(root, "state", "vapid", "private.pem"), "mailto:t@example.com")
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.DiscardHandler)
	h := dav.New(s, dav.Options{
		Prefix: "", UIPath: "", MaxResourceSize: 1 << 20, Photo: contact.PhotoPolicy{MaxSize: 0, Guard: false, Stabilize: false, ThumbnailSize: 0, Quality: 0},
		Birthdays: false, BirthdayAlarmDays: 0, BirthdayColor: "", AllowCollectionDelete: false, CalendarUserAddress: "",
		Push: push.New(keys, filepath.Join(root, "state"), log),
	}, log, nil)

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := &client{t: t, srv: srv}
	book := "/user/contacts/addressbook/"

	st, hdr, body := c.do("PROPFIND", book, `<d:propfind xmlns:d="DAV:" xmlns:p="https://bitfire.at/webdav-push"><d:prop><p:transports/><p:topic/><p:supported-triggers/></d:prop></d:propfind>`, "Depth", "0")
	expect(t, "push props", st, http.StatusMultiStatus, body)
	mustContain(t, "push props", body, `type="p256ecdsa">`+keys.Public, "<p:topic>", "<p:content-update><d:depth>1</d:depth>")

	if !strings.Contains(hdr.Get("DAV"), "webdav-push") {
		t.Fatalf("DAV header %q", hdr.Get("DAV"))
	}

	register := `<P:push-register xmlns:D="DAV:" xmlns:P="https://bitfire.at/webdav-push"><P:subscription><P:web-push-subscription>` +
		`<P:push-resource>` + pushSvc.URL + `/sub/1</P:push-resource>` +
		`<P:subscription-public-key type="p256dh">BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4</P:subscription-public-key>` +
		`<P:auth-secret>BTBZMqHH6r4Tts7J_aSIgg</P:auth-secret></P:web-push-subscription></P:subscription>` +
		`<P:trigger><P:content-update><D:depth>1</D:depth></P:content-update></P:trigger>` +
		`<P:expires>` + time.Now().Add(72*time.Hour).UTC().Format(http.TimeFormat) + `</P:expires></P:push-register>`

	st, hdr, body = c.do("POST", book, register, "Content-Type", "application/xml")
	expect(t, "register", st, http.StatusCreated, body)

	location := hdr.Get("Location")
	if !strings.HasPrefix(location, "/.subscriptions/") || hdr.Get("Expires") == "" {
		t.Fatalf("Location %q Expires %q", location, hdr.Get("Expires"))
	}

	st, _, body = c.do("POST", book, register)
	expect(t, "renew", st, http.StatusOK, body)

	st, _, body = c.do("PUT", book+"alice.vcf", alice)
	expect(t, "put", st, http.StatusCreated, body)

	select {
	case got := <-delivered:
		if got.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(got.Get("Authorization"), "vapid t=") {
			t.Fatalf("delivery headers %v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no push after a PUT")
	}

	st, _, body = c.do("PUT", book+"bob.vcf", bob, "Push-Dont-Notify", `"`+location+`"`)
	expect(t, "put with Push-Dont-Notify", st, http.StatusCreated, body)

	select {
	case <-delivered:
		t.Fatal("Push-Dont-Notify was ignored")
	case <-time.After(300 * time.Millisecond):
	}

	st, _, body = c.do("DELETE", location, "")
	expect(t, "unregister", st, http.StatusNoContent, body)

	st, _, body = c.do("DELETE", location, "")
	expect(t, "unregister twice", st, http.StatusNotFound, body)

	// The subscription file never becomes a collection member.
	_, _, body = c.do("PROPFIND", book, `<d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>`, "Depth", "1")
	if strings.Contains(body, "push-subscriptions") {
		t.Fatalf("subscription file listed as a member:\n%s", body)
	}
}
