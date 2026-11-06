package contact

import (
	"strings"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// New returns a minimal vCard 3.0 (the version iOS and DAVx5 write) with
// a UID and a formatted name.
func New(uid, fullName string) Card {
	return Card{Lines: []vcf.Line{
		line("BEGIN", "VCARD"),
		line("VERSION", V3),
		line("PRODID", "-//farvardin//EN"),
		line("UID", uid),
		line("FN", vcf.Escape(fullName)),
		line("N", ";"+vcf.Escape(fullName)+";;;"),
		line("END", "VCARD"),
	}, slim: nil}
}

func line(name, value string) vcf.Line {
	return vcf.Line{Group: "", Name: name, Params: nil, Value: value, Spelling: ""}
}

// Labelled is one value of a multi-valued property, with its type
// ("cell", "home", "work") or a free label.
type Labelled struct {
	Value string
	Type  string
}

// Replace removes every line of the property (and the X-ABLabel lines of
// their groups) and inserts lines in their place, or before END.
func (c *Card) Replace(name string, lines []vcf.Line) {
	groups := map[string]bool{}

	for _, l := range c.Lines {
		if l.Name == name && l.Group != "" {
			groups[l.Group] = true
		}
	}

	out := make([]vcf.Line, 0, len(c.Lines)+len(lines))
	placed := false

	for _, l := range c.Lines {
		drop := l.Name == name || (l.Group != "" && groups[l.Group] && l.Name == "X-ABLABEL")

		if drop {
			if !placed && l.Name == name {
				out = append(out, lines...)
				placed = true
			}

			continue
		}

		if l.Name == "END" && !placed {
			out = append(out, lines...)
			placed = true
		}

		out = append(out, l)
	}

	c.Lines = out
}

// SetText sets a single-valued text property; "" removes it.
func (c *Card) SetText(name, value string) {
	if value == "" {
		c.Replace(name, nil)

		return
	}

	c.Replace(name, []vcf.Line{line(name, vcf.Escape(value))})
}

// SetLabelled replaces every value of a multi-valued property (TEL, EMAIL,
// URL). Known vCard types go in TYPE; anything else becomes an Apple
// X-ABLabel so iOS shows it.
func (c *Card) SetLabelled(name string, values []Labelled) {
	known := map[string]bool{"home": true, "work": true, "cell": true, "mobile": true, "fax": true, "pager": true, "main": true, "other": true, "iphone": true}

	var (
		lines []vcf.Line
		n     int
	)

	for _, v := range values {
		if strings.TrimSpace(v.Value) == "" {
			continue
		}

		l := line(name, vcf.Escape(v.Value))
		t := strings.ToLower(strings.TrimSpace(v.Type))

		switch {
		case t == "mobile":
			l.SetParam("TYPE", "CELL")
		case known[t]:
			l.SetParam("TYPE", strings.ToUpper(t))
		case t != "":
			n++
			l.Group = "farv" + string(rune('a'+n%26))
			lines = append(lines, l, vcf.Line{Group: l.Group, Name: "X-ABLABEL", Params: nil, Value: vcf.Escape(v.Type), Spelling: "X-ABLabel"})

			continue
		}

		lines = append(lines, l)
	}

	c.Replace(name, lines)
}

// SetName sets FN and the structured N from given and family names.
func (c *Card) SetName(full, given, family string) {
	if full == "" {
		full = strings.TrimSpace(given + " " + family)
	}

	if full != "" {
		c.SetText("FN", full)
	}

	if given != "" || family != "" {
		n := c.Name()
		if given != "" {
			n.Given = given
		}

		if family != "" {
			n.Family = family
		}

		esc := func(s string) string { return strings.ReplaceAll(vcf.Escape(s), `\,`, ",") }
		c.Replace("N", []vcf.Line{line("N", esc(n.Family)+";"+esc(n.Given)+";"+esc(n.Additional)+";"+esc(n.Prefix)+";"+esc(n.Suffix))})
	}
}

// Touch sets REV to now, as clients do on every save.
func (c *Card) Touch(now time.Time) {
	c.Replace("REV", []vcf.Line{line("REV", now.UTC().Format("2006-01-02T15:04:05Z"))})
}
