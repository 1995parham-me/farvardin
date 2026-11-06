package contact

import (
	"strconv"
	"strings"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// Value is one labelled value of a multi-valued property (email, phone,
// URL, address).
type Value struct {
	Value     string
	Types     []string
	Label     string
	Preferred bool
}

// Name holds the structured N property.
type Name struct {
	Family, Given, Additional, Prefix, Suffix string
}

// DateKind tells a birthday from an anniversary.
type DateKind string

// Date kinds.
const (
	Birthday    DateKind = "birthday"
	Anniversary DateKind = "anniversary"
	OtherDate   DateKind = "other"
)

// Date is a calendar date that may have no year, as in "--0316".
type Date struct {
	Kind  DateKind
	Label string
	Year  int // 0 when unknown
	Month time.Month
	Day   int
}

// Name returns the structured name.
func (c Card) Name() Name {
	l, ok := c.Get("N")
	if !ok {
		return Name{}
	}

	parts := l.Components()
	for len(parts) < 5 {
		parts = append(parts, "")
	}

	return Name{Family: parts[0], Given: parts[1], Additional: parts[2], Prefix: parts[3], Suffix: parts[4]}
}

// DisplayName is FN, falling back to N, ORG, an email or the UID.
func (c Card) DisplayName() string {
	if fn := strings.TrimSpace(c.FN()); fn != "" {
		return fn
	}

	n := c.Name()
	if s := strings.TrimSpace(strings.Join([]string{n.Prefix, n.Given, n.Additional, n.Family, n.Suffix}, " ")); s != "" {
		return strings.Join(strings.Fields(s), " ")
	}

	if org := c.Organization(); org != "" {
		return org
	}

	if e := c.Values("EMAIL"); len(e) > 0 {
		return e[0].Value
	}

	return c.UID()
}

// Organization returns the first ORG component.
func (c Card) Organization() string {
	l, ok := c.Get("ORG")
	if !ok {
		return ""
	}

	return l.Components()[0]
}

// Values returns every value of a multi-valued property, with types,
// Apple labels and the preferred flag of both vCard versions resolved.
func (c Card) Values(name string) []Value {
	lines := c.All(name)
	out := make([]Value, 0, len(lines))

	for _, l := range lines {
		v := Value{Value: l.Text(), Types: nil, Label: c.Label(l), Preferred: false}

		for _, t := range l.Param("TYPE") {
			t = strings.ToLower(t)
			if t == "pref" {
				v.Preferred = true

				continue
			}

			v.Types = append(v.Types, t)
		}

		if len(l.Param("PREF")) > 0 {
			v.Preferred = true
		}

		if l.Name == "ADR" {
			v.Value = formatAddress(l)
		}

		out = append(out, v)
	}

	return out
}

func formatAddress(l vcf.Line) string {
	var parts []string

	for _, p := range l.Components() {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}

	return strings.Join(parts, ", ")
}

// Dates returns the birthday, the anniversary and Apple's labelled dates.
func (c Card) Dates() []Date {
	var out []Date

	for _, l := range c.Lines {
		var kind DateKind

		switch l.Name {
		case "BDAY":
			kind = Birthday
		case "ANNIVERSARY", "X-ANNIVERSARY":
			kind = Anniversary
		case "X-ABDATE":
			kind = OtherDate
		default:
			continue
		}

		d, ok := ParseDate(l.Value)
		if !ok {
			continue
		}

		d.Kind = kind
		if kind == OtherDate {
			d.Label = c.Label(l)
			if strings.EqualFold(d.Label, "Anniversary") {
				d.Kind = Anniversary
			}
		}

		out = append(out, d)
	}

	return out
}

// ParseDate reads the date forms used in vCards: 1990-03-16, 19900316,
// --0316, --03-16, 1604-03-16 (Apple's "no year") and date-times.
func ParseDate(s string) (Date, bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, 'T'); i > 0 {
		s = s[:i]
	}

	year := 0

	if rest, ok := strings.CutPrefix(s, "--"); ok {
		s = rest
	} else {
		digits := strings.ReplaceAll(s, "-", "")
		if len(digits) < 8 {
			return Date{}, false
		}

		y, err := strconv.Atoi(digits[:4])
		if err != nil {
			return Date{}, false
		}

		year, s = y, digits[4:]
	}

	s = strings.ReplaceAll(s, "-", "")
	if len(s) != 4 {
		return Date{}, false
	}

	month, err1 := strconv.Atoi(s[:2])
	day, err2 := strconv.Atoi(s[2:])

	if err1 != nil || err2 != nil || month < 1 || month > 12 || day < 1 || day > 31 {
		return Date{}, false
	}

	// Apple writes 1604 when the year is unknown.
	if year == 1604 {
		year = 0
	}

	return Date{Kind: OtherDate, Label: "", Year: year, Month: time.Month(month), Day: day}, true
}

// Next returns the next occurrence of the date on or after from.
func (d Date) Next(from time.Time) time.Time {
	y := from.Year()

	for {
		t := time.Date(y, d.Month, d.Day, 0, 0, 0, 0, from.Location())
		// Skip invalid days such as 29 February in a common year.
		if t.Month() == d.Month && !t.Before(truncateDay(from)) {
			return t
		}

		y++
	}
}

func truncateDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
