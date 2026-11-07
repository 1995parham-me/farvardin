package store_test

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/1995parham-me/farvardin/internal/infra/store"
)

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}

	return strings.TrimSpace(string(out))
}

func open(t *testing.T) (*store.Store, string) {
	t.Helper()

	root := t.TempDir()

	s, err := store.Open(root, "user", store.Author{Name: "Test", Email: "test@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.EnsureDefaults(); err != nil {
		t.Fatal(err)
	}

	return s, root
}

const card = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:a\r\nFN:A\r\nEND:VCARD\r\n"

func TestPutReadDeleteKeepsGitConsistent(t *testing.T) {
	t.Parallel()

	s, root := open(t)
	dir := filepath.Join(root, "user", "contacts", "addressbook")

	c, err := s.Collection(store.AddressBook, "addressbook")
	if err != nil {
		t.Fatal(err)
	}

	etag, created, err := c.Put("a.vcf", []byte(card), store.Condition{IfMatch: "", IfNoneMatch: true}, "test/1.0")
	if err != nil || !created {
		t.Fatalf("put: %v created=%v", err, created)
	}

	// The ETag is the git blob id.
	if want := gitCmd(t, dir, "hash-object", "a.vcf"); etag != want {
		t.Fatalf("etag %s, git says %s", etag, want)
	}

	if st := gitCmd(t, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("worktree not clean after put:\n%s", st)
	}

	if msg := gitCmd(t, dir, "log", "-1", "--format=%B"); !strings.Contains(msg, "Added a.vcf") || !strings.Contains(msg, "Requester: test/1.0") {
		t.Fatalf("unexpected message %q", msg)
	}

	if _, _, err := c.Put("a.vcf", []byte(card), store.Condition{IfMatch: "", IfNoneMatch: true}, ""); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("If-None-Match on an existing object: %v", err)
	}

	if _, _, err := c.Put("a.vcf", []byte(card+"\r\n"), store.Condition{IfMatch: "deadbeef", IfNoneMatch: false}, ""); !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("stale If-Match: %v", err)
	}

	snap, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	token := snap.Token

	if _, _, err := c.Put("b.vcf", []byte(strings.Replace(card, "UID:a", "UID:b", 1)), store.Condition{}, ""); err != nil {
		t.Fatal(err)
	}

	if err := c.Delete("a.vcf", store.Condition{IfMatch: etag, IfNoneMatch: false}, ""); err != nil {
		t.Fatal(err)
	}

	changed, deleted, _, err := c.Changes(token)
	if err != nil {
		t.Fatal(err)
	}

	if len(changed) != 1 || changed[0].Name != "b.vcf" || len(deleted) != 1 || deleted[0] != "a.vcf" {
		t.Fatalf("changes since %s: changed=%v deleted=%v", token, changed, deleted)
	}

	if _, _, _, err := c.Changes("0123456789012345678901234567890123456789"); !errors.Is(err, store.ErrInvalidToken) {
		t.Fatalf("unknown token: %v", err)
	}

	if st := gitCmd(t, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("worktree not clean after delete:\n%s", st)
	}

	gitCmd(t, dir, "fsck", "--strict")

	hist, err := c.History("a.vcf", 10)
	if err != nil || len(hist) != 2 || hist[0].ETag != "" || hist[1].Requester != "test/1.0" {
		t.Fatalf("history: %+v %v", hist, err)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	t.Parallel()

	m := store.ParseMeta([]byte("[DEFAULT]\ntype = calendar\ncolor = #FF9500\n\n[calendar]\norder = 1\n"))
	if m.Type() != "calendar" || m.Color() != "#FF9500" || m.Order() != 1 {
		t.Fatalf("parsed %+v", m)
	}

	m.Set("", "displayname", "Home")
	m.Set("", "color", "")

	got := store.ParseMeta(m.Encode())
	if got.DisplayName() != "Home" || got.Color() != "" || got.Order() != 1 {
		t.Fatalf("round trip: %s", m.Encode())
	}
}

func TestDeleted(t *testing.T) {
	t.Parallel()

	s, _ := open(t)

	c, err := s.Collection(store.AddressBook, "addressbook")
	if err != nil {
		t.Fatal(err)
	}

	put := func(name, data, ifMatch string) string {
		t.Helper()

		etag, _, err := c.Put(name, []byte(data), store.Condition{IfMatch: ifMatch, IfNoneMatch: ifMatch == ""}, "test/1.0")
		if err != nil {
			t.Fatal(err)
		}

		return etag
	}

	b1 := strings.Replace(card, "UID:a", "UID:b", 1)
	b2 := strings.Replace(b1, "FN:A", "FN:B", 1)

	put("a.vcf", card, "")
	e := put("b.vcf", b1, "")
	e = put("b.vcf", b2, e)

	if err := c.Delete("b.vcf", store.Condition{IfMatch: e, IfNoneMatch: false}, "test/1.0"); err != nil {
		t.Fatal(err)
	}

	gone, err := c.Deleted()
	if err != nil {
		t.Fatal(err)
	}

	if len(gone) != 1 || gone[0].Name != "b.vcf" || gone[0].Last != e || gone[0].Deleted.ETag != "" {
		t.Fatalf("deleted: %+v (last should be %s)", gone, e)
	}

	// Back under the same name: no longer deleted.
	put("b.vcf", b2, "")

	if gone, _ := c.Deleted(); len(gone) != 0 {
		t.Fatalf("after restore: %+v", gone)
	}
}
