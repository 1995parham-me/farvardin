package edits_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/dav"
	"github.com/1995parham-me/farvardin/internal/infra/edits"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

const who = "test"

func setup(t *testing.T) (edits.Editor, *store.Collection) {
	t.Helper()

	s, err := store.Open(t.TempDir(), "user", store.Author{Name: "T", Email: "t@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.EnsureDefaults(); err != nil {
		t.Fatal(err)
	}

	d := dav.New(s, dav.Options{ //nolint:exhaustruct // defaults
		Photo: contact.PhotoPolicy{MaxSize: 0, Guard: false, Stabilize: false, ThumbnailSize: 0, Quality: 0},
	}, slog.New(slog.DiscardHandler), nil)

	book, err := s.Collection(store.AddressBook, "addressbook")
	if err != nil {
		t.Fatal(err)
	}

	cache, full := d.Cache(), d.Full()

	return edits.Editor{
		Store: s, Parse: edits.Parser(cache), Full: edits.Parser(full),
		Write: d.WriteCard, Delete: d.DeleteObject, Regions: []string{"IR", "ES"},
	}, book
}

func vcard(uid, fn string, lines ...string) string {
	body := strings.Join(lines, "\r\n")
	if body != "" {
		body += "\r\n"
	}

	return "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:" + uid + "\r\nFN:" + fn + "\r\nN:;" + fn + ";;;\r\n" + body + "END:VCARD\r\n"
}

func put(t *testing.T, e edits.Editor, name, data, ifMatch string) string {
	t.Helper()

	etag, err := e.Write("addressbook", name, []byte(data), ifMatch, who)
	if err != nil {
		t.Fatal(err)
	}

	return etag
}

func read(t *testing.T, book *store.Collection, name string) string {
	t.Helper()

	data, _, err := book.Read(name)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func TestMerge(t *testing.T) {
	t.Parallel()

	e, book := setup(t)
	ka := put(t, e, "a.vcf", vcard("a", "Ali", "TEL:0912 345 6789"), "")
	kb := put(t, e, "b.vcf", vcard("b", "Ali Work", "TEL:+98 912 345 6789", "TEL:612 345 678", "EMAIL:ali@example.com"), "")

	groups, _, err := e.Duplicates("addressbook")
	if err != nil || len(groups) != 1 || strings.Join(groups[0].IDs, ",") != "a.vcf,b.vcf" {
		t.Fatalf("duplicates %+v %v", groups, err)
	}

	// A stale ETag is refused before anything is written.
	if _, err := e.Merge("addressbook", edits.Card{ID: "a.vcf", ETag: ka}, []edits.Card{{ID: "b.vcf", ETag: "stale"}}, who); !errors.Is(err, edits.ErrChanged) {
		t.Fatalf("stale merge: %v", err)
	}

	res, err := e.Merge("addressbook", edits.Card{ID: "a.vcf", ETag: ka}, []edits.Card{{ID: "b.vcf", ETag: kb}}, who)
	if err != nil || len(res.Merged) != 1 || res.Failed != nil {
		t.Fatalf("merge %+v %v", res, err)
	}

	got := read(t, book, "a.vcf")
	for _, want := range []string{"TEL:0912 345 6789\r\n", "TEL:612 345 678\r\n", "EMAIL:ali@example.com\r\n", "REV:"} {
		if !strings.Contains(got, want) {
			t.Errorf("merged card lacks %q:\n%s", want, got)
		}
	}

	if strings.Count(got, "TEL:") != 2 {
		t.Errorf("the shared number was copied:\n%s", got)
	}

	if _, _, err := book.Read("b.vcf"); err == nil {
		t.Fatal("b.vcf was not deleted")
	}

	if groups, _, _ := e.Duplicates("addressbook"); len(groups) != 0 {
		t.Fatalf("still duplicates: %+v", groups)
	}
}

func TestRestoreVersionAndDeleted(t *testing.T) {
	t.Parallel()

	e, book := setup(t)
	v1 := vcard("a", "Ali", "TEL:+989123456789")
	e1 := put(t, e, "a.vcf", v1, "")
	e2 := put(t, e, "a.vcf", vcard("a", "Ali", "NOTE:the phone dropped the number"), e1)

	if _, err := e.RestoreVersion("addressbook", "a.vcf", "0000", e2, who); !errors.Is(err, edits.ErrNotHistory) {
		t.Fatalf("revision outside the history: %v", err)
	}

	if _, err := e.RestoreVersion("addressbook", "a.vcf", e1, "stale", who); !errors.Is(err, edits.ErrChanged) {
		t.Fatalf("stale restore: %v", err)
	}

	e3, err := e.RestoreVersion("addressbook", "a.vcf", e1, e2, who)
	if err != nil || e3 != e1 || read(t, book, "a.vcf") != v1 {
		t.Fatalf("restore version: %s %v", e3, err)
	}

	if err := e.Delete(store.AddressBook, "addressbook", "a.vcf", e3, who); err != nil {
		t.Fatal(err)
	}

	gone, err := e.Deleted("addressbook")
	if err != nil || len(gone) != 1 || gone[0].Display != "Ali" || gone[0].Last != e1 {
		t.Fatalf("deleted %+v %v", gone, err)
	}

	if _, err := e.RestoreDeleted("addressbook", "a.vcf", who); err != nil || read(t, book, "a.vcf") != v1 {
		t.Fatalf("restore deleted: %v", err)
	}

	if _, err := e.RestoreDeleted("addressbook", "a.vcf", who); !errors.Is(err, edits.ErrBadRestore) {
		t.Fatalf("restore of a card that exists: %v", err)
	}
}

func TestFixAllPhones(t *testing.T) {
	t.Parallel()

	e, book := setup(t)
	put(t, e, "a.vcf", vcard("a", "Ali", "TEL:0912 345 6789"), "")
	put(t, e, "b.vcf", vcard("b", "Correos", "TEL:900 400 004"), "")
	put(t, e, "c.vcf", vcard("c", "Clean", "TEL:+34 612 345 678"), "")

	res, err := e.FixAllPhones("addressbook", who)
	if err != nil || len(res) != 2 || res[0].Error != "" || res[1].Error != "" {
		t.Fatalf("fix all: %+v %v", res, err)
	}

	if !strings.Contains(read(t, book, "a.vcf"), "TEL:+989123456789\r\n") || !strings.Contains(read(t, book, "b.vcf"), "TEL:+34900400004\r\n") {
		t.Fatal("numbers not fixed")
	}

	if res, _ := e.FixAllPhones("addressbook", who); len(res) != 0 {
		t.Fatalf("second run: %+v", res)
	}
}

func TestDeletedSkipsRenamed(t *testing.T) {
	t.Parallel()

	e, _ := setup(t)
	old := put(t, e, "old-name.vcf", vcard("a", "Ali"), "")
	gone := put(t, e, "gone.vcf", vcard("b", "Bita"), "")

	// A rename: the card leaves old-name.vcf and comes back as a.vcf.
	for name, etag := range map[string]string{"old-name.vcf": old, "gone.vcf": gone} {
		if err := e.Delete(store.AddressBook, "addressbook", name, etag, who); err != nil {
			t.Fatal(err)
		}
	}

	put(t, e, "a.vcf", vcard("a", "Ali"), "")
	// An import that gave a card a new UID.
	reimport := put(t, e, "c1.vcf", vcard("c1", "Cyrus"), "")

	if err := e.Delete(store.AddressBook, "addressbook", "c1.vcf", reimport, who); err != nil {
		t.Fatal(err)
	}

	put(t, e, "c2.vcf", vcard("c2", "cyrus "), "")

	list, err := e.Deleted("addressbook")
	if err != nil || len(list) != 1 || list[0].Name != "gone.vcf" {
		t.Fatalf("deleted %+v %v", list, err)
	}
}

func TestNotSame(t *testing.T) {
	t.Parallel()

	e, _ := setup(t)
	put(t, e, "a.vcf", vcard("a", "Saman", "TEL:+34 931 000 000"), "")
	put(t, e, "b.vcf", vcard("b", "Lost and Found", "TEL:+34 931 000 000"), "")

	if err := e.NotSame("addressbook", []string{"a.vcf"}); !errors.Is(err, edits.ErrNotGroup) {
		t.Fatalf("part of a group: %v", err)
	}

	if err := e.NotSame("addressbook", []string{"b.vcf", "a.vcf"}); err != nil {
		t.Fatal(err)
	}

	if groups, hidden, err := e.Duplicates("addressbook"); err != nil || len(groups) != 0 || hidden != 1 {
		t.Fatalf("after marking: %+v %d %v", groups, hidden, err)
	}

	// A third card joins: the group is back.
	put(t, e, "c.vcf", vcard("c", "Welcome Desk", "TEL:+34931000000"), "")

	if groups, _, _ := e.Duplicates("addressbook"); len(groups) != 1 || len(groups[0].IDs) != 3 {
		t.Fatalf("with a new card: %+v", groups)
	}

	if err := e.ShowAllDuplicates("addressbook"); err != nil {
		t.Fatal(err)
	}

	if _, hidden, _ := e.Duplicates("addressbook"); hidden != 0 {
		t.Fatalf("still hidden: %d", hidden)
	}
}
