package push

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()

	b, err := unb64(s)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// RFC 8291 appendix A.
func TestEncryptMatchesRFC8291Vector(t *testing.T) {
	t.Parallel()

	as, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := encryptWith(
		[]byte("When I grow up, I want to be a watermelon"),
		mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
		mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg"),
		as,
		mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw"),
	)
	if err != nil {
		t.Fatal(err)
	}

	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if b64(got) != want {
		t.Fatalf("encrypted body differs from RFC 8291:\n got %s\nwant %s", b64(got), want)
	}
}

func TestVAPIDHeaderVerifies(t *testing.T) {
	t.Parallel()

	keyPath := filepath.Join(t.TempDir(), "vapid", "private.pem")

	keys, err := LoadKeys(keyPath, "mailto:admin@example.com")
	if err != nil {
		t.Fatal(err)
	}

	h, err := keys.vapidHeader("https://fcm.googleapis.com/fcm/send/abc", time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}

	jwt, k, ok := strings.Cut(strings.TrimPrefix(h, "vapid t="), ", k=")
	if !ok || k != keys.Public {
		t.Fatalf("header %q", h)
	}

	parts := strings.Split(jwt, ".")
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])

	if !strings.Contains(string(claims), `"aud":"https://fcm.googleapis.com"`) || !strings.Contains(string(claims), `"sub":"mailto:admin@example.com"`) {
		t.Fatalf("claims %s", claims)
	}

	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])

	if !ecdsa.Verify(&keys.private.PublicKey, digest[:], r, s) {
		t.Fatal("VAPID signature does not verify")
	}

	// The key written on first use loads again to the same public key.
	again, err := LoadKeys(keyPath, "")
	if err != nil || again.Public != keys.Public {
		t.Fatalf("reloaded key differs: %v", err)
	}

	if fi, _ := os.Stat(keyPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
}

// A file in the exact format Xandikos writes.
const xandikosFile = `{
  "subscriptions": {
    "BmLCqq9OVEjcTFzsUEYtZw": {
      "auth_secret": "BTBZMqHH6r4Tts7J_aSIgg",
      "content_encoding": "aes128gcm",
      "expires": "2099-11-08T06:30:10+00:00",
      "id": "BmLCqq9OVEjcTFzsUEYtZw",
      "p256dh": "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
      "push_resource": "PUSH",
      "triggers": [
        {
          "depth": "1",
          "kind": "content-update",
          "props": []
        }
      ]
    }
  },
  "topic": "vM9QpsKgVl0LsAQCrGYfcA"
}`

func TestXandikosStateAndDelivery(t *testing.T) {
	t.Parallel()

	var (
		got     = make(chan *http.Request, 1)
		gotBody = make(chan []byte, 1)
	)

	pushSvc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- r
		gotBody <- body

		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(pushSvc.Close)

	dir, state := t.TempDir(), t.TempDir()
	file := strings.Replace(xandikosFile, "PUSH", pushSvc.URL+"/push/abc", 1)

	if err := os.WriteFile(filepath.Join(dir, SubscriptionsFile), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}

	keys, err := LoadKeys(filepath.Join(state, "vapid", "private.pem"), "mailto:admin@xandikos.local")
	if err != nil {
		t.Fatal(err)
	}

	svc := New(keys, state, slog.New(slog.DiscardHandler))
	svc.client = pushSvc.Client()

	topic, err := svc.Topic(dir)
	if err != nil || topic != "vM9QpsKgVl0LsAQCrGYfcA" {
		t.Fatalf("topic %q %v", topic, err)
	}

	// Loading and saving keeps the Xandikos file byte for byte.
	f, _ := svc.load(dir)
	if err := writeJSON(filepath.Join(dir, SubscriptionsFile), f); err != nil {
		t.Fatal(err)
	}

	if data, _ := os.ReadFile(filepath.Join(dir, SubscriptionsFile)); string(data) != file {
		t.Fatalf("round trip changed the file:\n%s", data)
	}

	svc.Notify(dir, Change{SyncToken: "b96b59fc", Properties: nil, SkipAll: false, Skip: nil})

	select {
	case r := <-got:
		body := <-gotBody
		if r.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") ||
			r.Header.Get("Topic") != topic || r.Header.Get("TTL") == "" || len(body) < 16+4+1+65+16 {
			t.Fatalf("bad delivery: %v (%d bytes)", r.Header, len(body))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no push message delivered")
	}

	// A message the subscriber asked to skip is not sent.
	svc.Notify(dir, Change{SyncToken: "x", Properties: nil, SkipAll: false, Skip: map[string]bool{"BmLCqq9OVEjcTFzsUEYtZw": true}})

	select {
	case <-got:
		t.Fatal("Push-Dont-Notify was ignored")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestRegisterRenewRemove(t *testing.T) {
	t.Parallel()

	dir, state := t.TempDir(), t.TempDir()

	keys, err := LoadKeys(filepath.Join(state, "vapid", "private.pem"), "")
	if err != nil {
		t.Fatal(err)
	}

	svc := New(keys, state, slog.New(slog.DiscardHandler))
	sub := Subscription{
		AuthSecret: "BTBZMqHH6r4Tts7J_aSIgg", ContentEncoding: "aes128gcm", Expires: nil, ID: "",
		P256DH: "BCVx", PushResource: "https://push.example/1",
		Triggers: []Trigger{{Depth: "1", Kind: ContentUpdate, Props: []string{}}},
	}

	first, created, err := svc.Register(dir, "/user/contacts/addressbook/", sub)
	if err != nil || !created || first.Expires == nil {
		t.Fatalf("register: %+v %v %v", first, created, err)
	}

	again, created, err := svc.Register(dir, "/user/contacts/addressbook/", sub)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("renew must keep the id: %+v %v %v", again, created, err)
	}

	if owner, err := svc.Owner(first.ID); err != nil || owner != "/user/contacts/addressbook/" {
		t.Fatalf("owner %q %v", owner, err)
	}

	var index map[string]string

	data, _ := os.ReadFile(filepath.Join(state, IndexFile))
	_ = json.Unmarshal(data, &index)

	if index[first.ID] == "" {
		t.Fatalf("index: %s", data)
	}

	if err := svc.Remove(dir, first.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Owner(first.ID); err == nil {
		t.Fatal("removed subscription still in the index")
	}

	skipAll, ids := ParseDontNotify(`"https://dav.example/.subscriptions/abc", def`)
	if skipAll || !ids["abc"] || !ids["def"] {
		t.Fatalf("Push-Dont-Notify: %v %v", skipAll, ids)
	}
}
