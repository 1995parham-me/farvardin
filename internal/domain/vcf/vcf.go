// Package vcf parses and writes the content-line format shared by vCard
// (RFC 6350, RFC 2426) and iCalendar (RFC 5545).
//
// The parser keeps every line in its original order, with its group,
// parameters and raw (still escaped) value, so a document can be edited one
// property at a time and written back without reordering anything else.
package vcf

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// ErrSyntax is returned for a line that has no name or no colon.
var ErrSyntax = errors.New("vcf: syntax error")

// Param is one property parameter, such as TYPE=cell,voice.
type Param struct {
	Name   string
	Values []string
	// Spelling is the name as it was written (for example "type"), kept
	// so a re-encoded line differs only where it was edited.
	Spelling string
}

// Line is one unfolded content line.
type Line struct {
	// Group is the optional property group, for example "item1".
	Group string
	// Name is the upper-case property name.
	Name   string
	Params []Param
	// Value is the raw value, still escaped as it was on the wire.
	Value string
	// Spelling is the name as it was written (for example "X-ABLabel").
	Spelling string

	// raw is the line exactly as it was read, with its folding and line
	// endings; orig is its unfolded text. Encode writes raw back when the
	// line still reads as orig, so untouched lines keep their bytes.
	raw  string
	orig string
}

// Param returns the values of the named parameter (case-insensitive).
func (l Line) Param(name string) []string {
	var out []string

	for _, p := range l.Params {
		if strings.EqualFold(p.Name, name) {
			out = append(out, p.Values...)
		}
	}

	return out
}

// HasParamValue reports whether the named parameter contains value
// (both compared case-insensitively).
func (l Line) HasParamValue(name, value string) bool {
	return slices.ContainsFunc(l.Param(name), func(v string) bool { return strings.EqualFold(v, value) })
}

// SetParam replaces every occurrence of the named parameter with one that
// carries values. With no values, the parameter is removed.
func (l *Line) SetParam(name string, values ...string) {
	l.Params = slices.DeleteFunc(l.Params, func(p Param) bool { return strings.EqualFold(p.Name, name) })
	if len(values) > 0 {
		l.Params = append(l.Params, Param{Name: strings.ToUpper(name), Values: values, Spelling: ""})
	}
}

// Text returns the value with RFC 6350 escapes removed.
func (l Line) Text() string { return Unescape(l.Value) }

// Components splits a structured value (N, ADR, ORG) on unescaped
// semicolons and unescapes each part.
func (l Line) Components() []string {
	parts := SplitUnescaped(l.Value, ';')
	for i := range parts {
		parts[i] = Unescape(parts[i])
	}

	return parts
}

// Parse unfolds data and splits it into content lines. Empty lines are
// skipped. Both CRLF and bare LF line endings are accepted.
func Parse(data []byte) ([]Line, error) {
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")

	var (
		lines   []Line
		current strings.Builder
		raw     strings.Builder
		lineNo  int
	)

	flush := func() error {
		if current.Len() == 0 {
			raw.Reset()

			return nil
		}

		l, err := ParseLine(current.String())
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}

		l.raw, l.orig = raw.String(), current.String()
		lines = append(lines, l)

		current.Reset()
		raw.Reset()

		return nil
	}

	for len(text) > 0 {
		lineNo++

		physical, rest, found := strings.Cut(text, "\n")
		text = rest

		withEnd := physical
		if found {
			withEnd += "\n"
		}

		content := strings.TrimSuffix(physical, "\r")

		if len(content) > 0 && (content[0] == ' ' || content[0] == '\t') && current.Len() > 0 {
			current.WriteString(content[1:])
			raw.WriteString(withEnd)

			continue
		}

		if err := flush(); err != nil {
			return nil, err
		}

		current.WriteString(content)
		raw.WriteString(withEnd)
	}

	if err := flush(); err != nil {
		return nil, err
	}

	return lines, nil
}

// ParseLine parses one unfolded content line.
func ParseLine(s string) (Line, error) {
	var l Line

	// The name (and group) ends at the first ';' or ':' outside quotes.
	nameEnd := strings.IndexAny(s, ";:")
	if nameEnd <= 0 {
		return l, fmt.Errorf("%w: %q", ErrSyntax, truncate(s))
	}

	name := s[:nameEnd]
	if group, prop, ok := strings.Cut(name, "."); ok {
		l.Group, name = group, prop
	}

	l.Name = strings.ToUpper(name)
	l.Spelling = name
	rest := s[nameEnd:]

	for strings.HasPrefix(rest, ";") {
		rest = rest[1:]

		p, after, err := parseParam(rest)
		if err != nil {
			return l, err
		}

		l.Params = append(l.Params, p)
		rest = after
	}

	if !strings.HasPrefix(rest, ":") {
		return l, fmt.Errorf("%w: missing ':' in %q", ErrSyntax, truncate(s))
	}

	l.Value = rest[1:]

	return l, nil
}

func parseParam(s string) (Param, string, error) {
	eq := strings.IndexAny(s, "=;:")
	if eq < 0 {
		return Param{}, "", fmt.Errorf("%w: bad parameter %q", ErrSyntax, truncate(s))
	}

	p := Param{Name: strings.ToUpper(s[:eq]), Values: nil, Spelling: s[:eq]}

	if s[eq] != '=' {
		// vCard 2.1 style bare parameter, e.g. ";PREF" or ";HOME".
		p.Values = []string{p.Spelling}
		p.Name, p.Spelling = "TYPE", ""

		return p, s[eq:], nil
	}

	s = s[eq+1:]

	for {
		var value string

		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				return p, "", fmt.Errorf("%w: unterminated quote", ErrSyntax)
			}

			value, s = s[1:end+1], s[end+2:]
		} else {
			end := strings.IndexAny(s, ",;:")
			if end < 0 {
				return p, "", fmt.Errorf("%w: parameter without value end", ErrSyntax)
			}

			value, s = s[:end], s[end:]
		}

		p.Values = append(p.Values, value)

		if !strings.HasPrefix(s, ",") {
			return p, s, nil
		}

		s = s[1:]
	}
}

// Encode writes lines with CRLF endings, folded at 75 octets without
// splitting a UTF-8 sequence.
func Encode(lines []Line) []byte {
	var b bytes.Buffer

	for _, l := range lines {
		text := l.String()

		if l.raw != "" && text == l.orig {
			b.WriteString(l.raw)

			if !strings.HasSuffix(l.raw, "\n") {
				b.WriteString("\r\n")
			}

			continue
		}

		writeFolded(&b, text)
	}

	return b.Bytes()
}

// String returns the unfolded line.
func (l Line) String() string {
	var b strings.Builder

	if l.Group != "" {
		b.WriteString(l.Group)
		b.WriteByte('.')
	}

	b.WriteString(spell(l.Name, l.Spelling))

	for _, p := range l.Params {
		b.WriteByte(';')
		b.WriteString(spell(p.Name, p.Spelling))
		b.WriteByte('=')

		for i, v := range p.Values {
			if i > 0 {
				b.WriteByte(',')
			}

			if strings.ContainsAny(v, ",;:") {
				b.WriteByte('"')
				b.WriteString(v)
				b.WriteByte('"')
			} else {
				b.WriteString(v)
			}
		}
	}

	b.WriteByte(':')
	b.WriteString(l.Value)

	return b.String()
}

// spell returns the original spelling while it still names the same
// property (a rename such as X-SOCIALPROFILE to SOCIALPROFILE drops it).
func spell(name, spelling string) string {
	if spelling != "" && strings.EqualFold(spelling, name) {
		return spelling
	}

	return name
}

const foldAt = 75

func writeFolded(b *bytes.Buffer, s string) {
	limit := foldAt

	for len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}

		b.WriteString(s[:cut])
		b.WriteString("\r\n ")

		s = s[cut:]
		limit = foldAt - 1
	}

	b.WriteString(s)
	b.WriteString("\r\n")
}

// Unescape removes RFC 6350 / RFC 5545 text escapes.
func Unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}

	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i == len(s)-1 {
			b.WriteByte(s[i])

			continue
		}

		i++

		switch s[i] {
		case 'n', 'N':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
		}
	}

	return b.String()
}

// Escape adds RFC 6350 text escapes.
func Escape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, ",", `\,`, ";", `\;`)

	return r.Replace(s)
}

// SplitUnescaped splits s on sep where sep is not preceded by a backslash.
// The parts are returned still escaped.
func SplitUnescaped(s string, sep byte) []string {
	var (
		parts []string
		start int
	)

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case sep:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}

	return append(parts, s[start:])
}

func truncate(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}

	return s
}
