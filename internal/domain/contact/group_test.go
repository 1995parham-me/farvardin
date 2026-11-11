package contact_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
)

// What DAVx5 uploads for a group when its account keeps groups as
// separate vCards (iOS writes the same).
const davx5Group = "BEGIN:VCARD\r\n" +
	"VERSION:3.0\r\n" +
	"PRODID:+//IDN bitfire.at//DAVx5/4.4 ez-vcard/0.12.1\r\n" +
	"UID:grp-1\r\n" +
	"FN:Home Services\r\n" +
	"N:Home Services;;;;\r\n" +
	"X-ADDRESSBOOKSERVER-KIND:group\r\n" +
	"X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:a\r\n" +
	"X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:b\r\n" +
	"X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:a\r\n" +
	"END:VCARD\r\n"

func TestGroupMembers(t *testing.T) {
	t.Parallel()

	g, err := contact.Parse([]byte(davx5Group))
	if err != nil {
		t.Fatal(err)
	}

	if !contact.IsGroup(g) {
		t.Fatal("a KIND:group card is a group")
	}

	if got := g.Members(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("members %v", got)
	}

	g.SetMembers([]string{"b", "urn:uuid:c", "c"})

	if got := g.Members(); !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("after set: %v", got)
	}

	if s := string(g.Encode()); strings.Count(s, "X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:") != 2 || strings.Contains(s, "urn:uuid:a") {
		t.Fatalf("encoded:\n%s", s)
	}

	if errs := contact.Validate(g); len(errs) != 0 {
		t.Fatalf("a group card must validate: %v", errs)
	}
}

func TestNewGroupIsAppleShaped(t *testing.T) {
	t.Parallel()

	g := contact.NewGroup("u1", "Family")
	g.SetMembers([]string{"x"})
	s := string(g.Encode())

	for _, want := range []string{"FN:Family\r\n", "N:Family;;;;\r\n", "X-ADDRESSBOOKSERVER-KIND:group\r\n", "X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:x\r\n"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in\n%s", want, s)
		}
	}

	v4, err := contact.Convert(g, contact.V4)
	if err != nil {
		t.Fatal(err)
	}

	if !contact.IsGroup(v4) || !slices.Equal(v4.Members(), []string{"x"}) {
		t.Fatalf("vCard 4.0 group:\n%s", v4.Encode())
	}

	g.SetGroupName("Kin")

	if g.DisplayName() != "Kin" || !strings.Contains(string(g.Encode()), "N:Kin;;;;\r\n") {
		t.Fatalf("renamed:\n%s", g.Encode())
	}
}

func TestCategories(t *testing.T) {
	t.Parallel()

	c, err := contact.Parse([]byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:x\r\nFN:A\r\nN:;A;;;\r\nCATEGORIES:Family, Home\\, Garden ,Family\r\nCATEGORIES:Work\r\nEND:VCARD\r\n"))
	if err != nil {
		t.Fatal(err)
	}

	if got := c.Categories(); !slices.Equal(got, []string{"Family", "Home, Garden", "Work"}) {
		t.Fatalf("categories %q", got)
	}

	if contact.IsGroup(c) {
		t.Fatal("a person is not a group")
	}
}
