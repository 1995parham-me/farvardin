package contact

import (
	"strings"
	"unicode"

	"github.com/nyaruka/phonenumbers"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// Phone numbers are read against a default region (an ISO 3166-1 code
// such as "IR"): it is the country of a number written without a country
// code, as in "0912 345 6789". With no region, only numbers that carry a
// country code ("+98 ...", "0098 ...") can be read.

// ValidRegion reports whether region is a country code the phone number
// metadata knows.
func ValidRegion(region string) bool {
	return phonenumbers.GetSupportedRegions()[strings.ToUpper(region)]
}

// stripScheme removes the "tel:" of a vCard 4.0 URI value.
func stripScheme(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if len(v) >= 4 && strings.EqualFold(v[:4], "tel:") {
		return v[4:], true
	}

	return v, false
}

// telNumber returns the number of a TEL line without the "tel:" scheme
// that vCard 4.0 uses for VALUE=uri, and whether the scheme was there.
func telNumber(l vcf.Line) (string, bool) {
	return stripScheme(l.Text())
}

// hasCountryCode reports whether a number is written in international
// form: "+98 ..." or "0098 ...".
func hasCountryCode(s string) bool {
	s = strings.TrimLeft(s, " (")

	return strings.HasPrefix(s, "+") || strings.HasPrefix(s, "00")
}

// serviceNumber reports numbers that are not ordinary phone numbers and
// are left alone: USSD and star codes ("*141#"), short codes ("110"),
// and vanity numbers with letters ("1-800-FLOWERS").
func serviceNumber(s string) bool {
	if strings.ContainsAny(s, "*#") || strings.IndexFunc(s, unicode.IsLetter) >= 0 && !strings.Contains(strings.ToLower(s), "ext") {
		return true
	}

	n := 0

	for _, r := range s {
		if r >= '0' && r <= '9' {
			n++
		}
	}

	return n < 6
}

// readTel parses a number. It returns nil when the number cannot be read.
func readTel(value, region string) *phonenumbers.PhoneNumber {
	v, _ := stripScheme(value)
	if region == "" && strings.HasPrefix(v, "00") {
		v = "+" + v[2:]
	}

	num, err := phonenumbers.Parse(v, strings.ToUpper(region))
	if err != nil {
		return nil
	}

	return num
}

// NormalizeTel returns the E.164 form of a phone number ("+989123456789"),
// reading a number without a country code in region. ok is false for a
// number that is not valid, a number with an extension (E.164 has no place
// for it) and service numbers.
func NormalizeTel(value, region string) (string, bool) {
	value, _ = stripScheme(value)
	if serviceNumber(value) {
		return "", false
	}

	num := readTel(value, region)
	if num == nil || num.GetExtension() != "" || !phonenumbers.IsValidNumber(num) {
		return "", false
	}

	return phonenumbers.Format(num, phonenumbers.E164), true
}

// PhoneKeys returns the digit strings a search for this number should
// match: the digits as written, the E.164 digits, and the national form
// ("09123456789"). A search for any of them finds the number whichever
// way it was written.
func PhoneKeys(value, region string) []string {
	keys := []string{phonenumbers.NormalizeDigitsOnly(value)}

	if num := readTel(value, region); num != nil && phonenumbers.IsValidNumber(num) {
		keys = append(keys,
			strings.TrimPrefix(phonenumbers.Format(num, phonenumbers.E164), "+"),
			phonenumbers.NormalizeDigitsOnly(phonenumbers.Format(num, phonenumbers.NATIONAL)),
		)
	}

	return keys
}

// LintPhones reports phone numbers that do not work everywhere: a number
// with no country code (it calls a different place, or nothing, from
// abroad), a number that is not valid, and the same number twice. A
// number without a country code gets a Fix when region is set.
func LintPhones(c Card, region string) []Issue {
	var issues []Issue

	seen := map[string]string{}

	for _, l := range c.All("TEL") {
		raw, _ := telNumber(l)
		if raw == "" || serviceNumber(raw) {
			continue
		}

		intl := hasCountryCode(raw)
		num := readTel(raw, region)
		valid := num != nil && phonenumbers.IsValidNumber(num)

		switch {
		case valid && num.GetExtension() != "":
			// E.164 cannot carry the extension; leave the number as it is.
		case valid && !intl:
			e164 := phonenumbers.Format(num, phonenumbers.E164)
			issues = append(issues, Issue{Severity: Warning, Message: "TEL " + quote(raw) + " has no country code", Fix: e164})
		case !valid && intl:
			issues = append(issues, Issue{Severity: Warning, Message: "TEL " + quote(raw) + " is not a valid phone number", Fix: ""})
		case !valid && region != "":
			issues = append(issues, Issue{Severity: Warning, Message: "TEL " + quote(raw) + " is not a valid phone number in " + strings.ToUpper(region), Fix: ""})
		}

		if !valid {
			continue
		}

		key := phonenumbers.Format(num, phonenumbers.E164)
		if first, dup := seen[key]; dup {
			issues = append(issues, Issue{Severity: Warning, Message: "TEL " + quote(raw) + " is the same number as " + quote(first), Fix: ""})
		} else {
			seen[key] = raw
		}
	}

	return issues
}

// PhoneFix is one number FixPhones changed.
type PhoneFix struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// FixPhones returns a copy of the card with every number that has no
// country code written in E.164, the fixes LintPhones suggests. A vCard
// 4.0 tel: URI keeps its scheme. Types, labels and every other line keep
// their bytes.
func FixPhones(c Card, region string) (Card, []PhoneFix) {
	out := c.Clone()

	var fixes []PhoneFix

	for i, l := range out.Lines {
		if l.Name != "TEL" {
			continue
		}

		raw, uri := telNumber(l)
		if hasCountryCode(raw) {
			continue
		}

		e164, ok := NormalizeTel(raw, region)
		if !ok {
			continue
		}

		v := e164
		if uri {
			v = "tel:" + e164
		}

		out.Lines[i].Value = vcf.Escape(v)
		fixes = append(fixes, PhoneFix{From: raw, To: e164})
	}

	return out, fixes
}

// Lint is Validate plus the phone number checks. Validate alone guards
// writes; Lint is for reports.
func Lint(c Card, region string) []Issue {
	return append(Validate(c), LintPhones(c, region)...)
}

func quote(s string) string { return `"` + s + `"` }
