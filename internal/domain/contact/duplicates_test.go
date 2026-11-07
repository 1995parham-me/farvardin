package contact_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
)

func card(uid, fn string, lines ...string) string {
	return "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:" + uid + "\r\nFN:" + fn + "\r\nN:;" + fn + ";;;\r\n" +
		strings.Join(lines, "\r\n") + map[bool]string{true: "\r\n", false: ""}[len(lines) > 0] + "END:VCARD\r\n"
}

func TestDuplicates(t *testing.T) {
	t.Parallel()

	entries := []contact.Entry{
		{ID: "a.vcf", Card: mustParse(t, card("a", "Ali", "TEL:0912 345 6789"))},
		{ID: "b.vcf", Card: mustParse(t, card("b", "Ali Work", "TEL:+98 912 345 6789", "EMAIL:ali@example.com"))},
		{ID: "c.vcf", Card: mustParse(t, card("c", "Mr Ali", "EMAIL:ALI@example.com"))},
		{ID: "d.vcf", Card: mustParse(t, card("d", "Saman Bank", "TEL:+98300086492"))},
		{ID: "e.vcf", Card: mustParse(t, card("e", "Saman", "TEL:983-000-86492"))},
		{ID: "f.vcf", Card: mustParse(t, card("f", "Irancell", "TEL:110", "TEL:My_Irancell"))},
		{ID: "g.vcf", Card: mustParse(t, card("g", "Police", "TEL:110"))},
		{ID: "h.vcf", Card: mustParse(t, card("h", "Irancell Club", "TEL:my irancell"))},
		{ID: "i.vcf", Card: mustParse(t, card("i", "saman bank"))},
		{ID: "j.vcf", Card: mustParse(t, card("j", "Alone", "TEL:+34 612 345 678"))},
	}

	got := contact.Duplicates(entries, []string{"IR"})

	ids := make([][]string, 0, len(got))
	for _, g := range got {
		ids = append(ids, g.IDs)
	}

	want := [][]string{{"a.vcf", "b.vcf", "c.vcf"}, {"f.vcf", "h.vcf"}, {"d.vcf", "e.vcf", "i.vcf"}}

	if !slices.EqualFunc(ids, want, slices.Equal) {
		t.Fatalf("groups %q, want %q", ids, want)
	}

	if r := got[0].Reasons; !slices.Equal(r, []string{"same number +989123456789", "same email ali@example.com"}) {
		t.Fatalf("reasons %q", r)
	}

	if r := got[2].Reasons; !slices.Equal(r, []string{"same number +98300086492", "same name \"Saman Bank\""}) {
		t.Fatalf("reasons %q", r)
	}
}

func TestDuplicatesColleagues(t *testing.T) {
	t.Parallel()

	entries := []contact.Entry{
		{ID: "co.vcf", Card: mustParse(t, card("co", "Leolando", "ORG:Leolando;", "X-ABShowAs:COMPANY", "EMAIL:soroush@leolando.com", "TEL:+34 931 000 000"))},
		{ID: "ceo.vcf", Card: mustParse(t, card("ceo", "Soroush", "ORG:Leolando", "EMAIL:soroush@leolando.com", "TEL:+34 931 000 000"))},
		{ID: "dev.vcf", Card: mustParse(t, card("dev", "Sara", "ORG:leolando", "TEL:+34 931 000 000"))},
		// Saved twice: the same name still joins them.
		{ID: "dev2.vcf", Card: mustParse(t, card("dev2", "Sara", "ORG:Leolando", "TEL:+34 931 000 000"))},
		// Not of the company: the shared number joins it.
		{ID: "x.vcf", Card: mustParse(t, card("x", "Someone", "ORG:Other", "TEL:+34 931 000 000"))},
		{ID: "org.vcf", Card: mustParse(t, card("org", "Acme", "KIND:org", "EMAIL:info@acme.example"))},
		{ID: "p.vcf", Card: mustParse(t, card("p", "Bob", "ORG:Acme", "EMAIL:info@acme.example"))},
	}

	got := contact.Duplicates(entries, []string{"ES"})
	if len(got) != 1 || !slices.Equal(got[0].IDs, []string{"co.vcf", "ceo.vcf", "dev.vcf", "dev2.vcf", "x.vcf"}) {
		t.Fatalf("groups %+v", got)
	}

	without := slices.DeleteFunc(slices.Clone(entries), func(e contact.Entry) bool { return e.ID == "x.vcf" })

	got = contact.Duplicates(without, []string{"ES"})
	if len(got) != 1 || !slices.Equal(got[0].IDs, []string{"dev.vcf", "dev2.vcf"}) || !slices.Equal(got[0].Reasons, []string{"same name \"Sara\""}) {
		t.Fatalf("colleagues joined: %+v", got)
	}
}

func TestMerge(t *testing.T) {
	t.Parallel()

	keep := mustParse(t, card("k", "Ali",
		"TEL;TYPE=cell:0912 345 6789",
		"item1.EMAIL:ali@example.com",
		"item1.X-ABLabel:Home",
		"NOTE:met in Tehran",
	))
	other := mustParse(t, card("o", "Ali Work",
		"TEL;TYPE=cell:+98 912 345 6789",
		"TEL;TYPE=work:+34 612 345 678",
		"item1.EMAIL:ALI@EXAMPLE.COM",
		"item1.X-ABLabel:Old",
		"item2.URL:https://ali.example",
		"item2.X-ABLabel:Blog",
		"BDAY:1990-05-01",
		"ORG:Acme",
		"NOTE:moved to Barcelona",
	))

	got := string(contact.Merge(keep, other, []string{"IR"}).Encode())

	want := card("k", "Ali",
		"TEL;TYPE=cell:0912 345 6789",
		"item1.EMAIL:ali@example.com",
		"item1.X-ABLabel:Home",
		"NOTE:met in Tehran\\n\\nmoved to Barcelona",
		"TEL;TYPE=work:+34 612 345 678",
		"item2.URL:https://ali.example",
		"item2.X-ABLabel:Blog",
		"BDAY:1990-05-01",
		"ORG:Acme",
	)

	if got != want {
		t.Fatalf("merged:\n%s\nwant:\n%s", got, want)
	}

	// Merging the same card again adds nothing.
	again := contact.Merge(mustParse(t, got), other, []string{"IR"})
	if string(again.Encode()) != got {
		t.Fatalf("second merge changed the card:\n%s", again.Encode())
	}
}
