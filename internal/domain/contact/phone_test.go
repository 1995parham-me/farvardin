package contact_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
)

// regions splits "IR,ES" into a region list; "" is none.
func regions(s string) []string {
	if s == "" {
		return nil
	}

	return strings.Split(s, ",")
}

func TestNormalizeTel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, region, want string
		ok               bool
	}{
		{"0912 345 6789", "IR", "+989123456789", true},
		{"+98 (912) 345-6789", "", "+989123456789", true},
		{"0098 912 345 6789", "", "+989123456789", true},
		{"0098 912 345 6789", "IR", "+989123456789", true},
		{"tel:+98-912-345-6789", "", "+989123456789", true},
		{"(201) 555-0123", "US", "+12015550123", true},
		{"989-999-920000", "IR", "+989999920000", true}, // country code without +
		{"+9810001884", "IR", "", false},                // SMS panel sender
		{"0912 345 6789", "", "", false},                // no region, no country code
		{"*141#", "IR", "", false},                      // USSD code
		{"110", "IR", "", false},                        // short code
		{"+98 000 123 4567", "", "", false},             // not valid
		{"+1 201 555 0123 ext. 12", "", "", false},      // E.164 has no extension
		// Two regions: a number is read in the one where it is valid.
		{"0912 345 6789", "IR,ES", "+989123456789", true},
		{"9123456789", "IR,ES", "+989123456789", true},
		{"612 345 678", "IR,ES", "+34612345678", true},
		{"900 701 149", "IR,ES", "+34900701149", true},
		{"612 345 678", "ES,IR", "+34612345678", true},
		{"0912 345 6789", "ES,IR", "+989123456789", true},
		{"+34 612 345 678", "IR", "+34612345678", true},
		{"989-999-920000", "IR,ES", "+989999920000", true},
		// Valid in Spain, but it begins with Iran's 98: an Iranian
		// sender saved without its "+", so it is not guessed.
		{"982-000-256", "IR,ES", "", false},
		{"986 715 014", "IR,ES", "", false},
		{"982-000-256", "ES", "+34982000256", true}, // ES alone cannot tell
	}

	for _, c := range cases {
		got, ok := contact.NormalizeTel(c.in, regions(c.region))
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeTel(%q, %q) = %q, %v; want %q, %v", c.in, c.region, got, ok, c.want, c.ok)
		}
	}
}

const phones3 = "BEGIN:VCARD\r\n" +
	"VERSION:3.0\r\n" +
	"UID:p\r\n" +
	"FN:Phones\r\n" +
	"N:;Phones;;;\r\n" +
	"item1.TEL;type=CELL:0912 345 6789\r\n" +
	"item1.X-ABLabel:Persian\r\n" +
	"TEL;type=WORK:+98 21 8888 7777\r\n" +
	"TEL;type=HOME:+989123456789\r\n" +
	"TEL:*141#\r\n" +
	"TEL:+98 000 123 4567\r\n" +
	"TEL:+9810001884\r\n" + // an SMS panel sender: unknown, not reported
	"END:VCARD\r\n"

func messages(issues []contact.Issue) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.String())
	}

	return out
}

func TestLintPhones(t *testing.T) {
	t.Parallel()

	c := mustParse(t, phones3)

	got := messages(contact.LintPhones(c, []string{"IR"}))
	want := []string{
		`warning: TEL "0912 345 6789" is not in international form (fix: +989123456789)`,
		`warning: TEL "+989123456789" is the same number as "0912 345 6789"`,
	}

	if !slices.Equal(got, want) {
		t.Fatalf("with region:\n got %q\nwant %q", got, want)
	}

	// Without a region a national number cannot be read: it is not
	// reported, and there is no duplicate to find.
	if got := messages(contact.LintPhones(c, nil)); len(got) != 0 {
		t.Fatalf("without region: %q", got)
	}
}

func TestFixPhones(t *testing.T) {
	t.Parallel()

	c := mustParse(t, phones3)

	fixed, fixes := contact.FixPhones(c, []string{"IR"})
	if len(fixes) != 1 || fixes[0] != (contact.PhoneFix{From: "0912 345 6789", To: "+989123456789"}) {
		t.Fatalf("fixes: %+v", fixes)
	}

	// Only the fixed line changes; its group, type and label stay.
	want := strings.Replace(phones3, "CELL:0912 345 6789", "CELL:+989123456789", 1)
	if got := string(fixed.Encode()); got != want {
		t.Fatalf("encoded:\n%s\nwant:\n%s", got, want)
	}

	if issues := contact.LintPhones(fixed, []string{"IR"}); len(issues) != 1 {
		t.Fatalf("after fix: %q", messages(issues))
	}

	if _, fixes := contact.FixPhones(c, nil); len(fixes) != 0 {
		t.Fatalf("no region: %+v", fixes)
	}
}

func TestFixPhonesKeepsTelURI(t *testing.T) {
	t.Parallel()

	c := mustParse(t, "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:q\r\nFN:Q\r\nTEL;VALUE=uri;TYPE=cell:tel:0912-345-6789\r\nEND:VCARD\r\n")

	fixed, fixes := contact.FixPhones(c, []string{"IR"})
	if len(fixes) != 1 || !strings.Contains(string(fixed.Encode()), "TEL;VALUE=uri;TYPE=cell:tel:+989123456789\r\n") {
		t.Fatalf("fixes %+v:\n%s", fixes, fixed.Encode())
	}
}

func TestPhoneKeys(t *testing.T) {
	t.Parallel()

	keys := contact.PhoneKeys("+98 912 345 6789", []string{"IR"})
	for _, want := range []string{"989123456789", "09123456789"} {
		if !slices.Contains(keys, want) {
			t.Errorf("keys %q lack %q", keys, want)
		}
	}
}

func TestFixPhonesTwoRegions(t *testing.T) {
	t.Parallel()

	c := mustParse(t, "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:m\r\nFN:M\r\n"+
		"TEL:0912 345 6789\r\n"+
		"TEL:612 345 678\r\n"+
		"TEL:982-000-256\r\n"+
		"END:VCARD\r\n")

	_, fixes := contact.FixPhones(c, []string{"IR", "ES"})
	want := []contact.PhoneFix{{From: "0912 345 6789", To: "+989123456789"}, {From: "612 345 678", To: "+34612345678"}}

	if !slices.Equal(fixes, want) {
		t.Fatalf("fixes %+v, want %+v", fixes, want)
	}
}
