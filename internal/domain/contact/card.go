// Package contact models vCards: reading their fields, checking them,
// converting between vCard 3.0, vCard 4.0 (RFC 6350 + RFC 9554) and
// JSContact (RFC 9553), and handling their photos.
package contact

import (
	"errors"
	"fmt"
	"strings"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// Errors returned by the parser.
var (
	ErrNoCard       = errors.New("no vCard found")
	ErrManyCards    = errors.New("more than one vCard in one resource")
	ErrUnterminated = errors.New("vCard is missing END:VCARD")
)

// Card is one vCard. Lines include the BEGIN and END lines, in their
// original order.
type Card struct {
	Lines []vcf.Line
}

// ParseAll returns every vCard found in data.
func ParseAll(data []byte) ([]Card, error) {
	lines, err := vcf.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing vCard: %w", err)
	}

	var (
		cards   []Card
		current *Card
	)

	for _, l := range lines {
		switch {
		case l.Name == "BEGIN" && strings.EqualFold(l.Value, "VCARD"):
			current = &Card{Lines: []vcf.Line{l}}
		case current == nil:
			// Lines outside a card are ignored.
		case l.Name == "END" && strings.EqualFold(l.Value, "VCARD"):
			current.Lines = append(current.Lines, l)
			cards = append(cards, *current)
			current = nil
		default:
			current.Lines = append(current.Lines, l)
		}
	}

	if current != nil {
		return cards, ErrUnterminated
	}

	if len(cards) == 0 {
		return nil, ErrNoCard
	}

	return cards, nil
}

// Parse returns the single vCard in data.
func Parse(data []byte) (Card, error) {
	cards, err := ParseAll(data)
	if err != nil {
		return Card{}, err
	}

	if len(cards) > 1 {
		return Card{}, ErrManyCards
	}

	return cards[0], nil
}

// Encode writes the card with CRLF line endings and folding.
func (c Card) Encode() []byte { return vcf.Encode(c.Lines) }

// Get returns the first line with the given property name.
func (c Card) Get(name string) (vcf.Line, bool) {
	for _, l := range c.Lines {
		if l.Name == name {
			return l, true
		}
	}

	return vcf.Line{}, false
}

// All returns every line with the given property name.
func (c Card) All(name string) []vcf.Line {
	var out []vcf.Line

	for _, l := range c.Lines {
		if l.Name == name {
			out = append(out, l)
		}
	}

	return out
}

// Value returns the unescaped text of the first property with that name.
func (c Card) Value(name string) string {
	l, ok := c.Get(name)
	if !ok {
		return ""
	}

	return l.Text()
}

// Version returns the VERSION value, for example "3.0" or "4.0".
func (c Card) Version() string { return c.Value("VERSION") }

// UID returns the UID value.
func (c Card) UID() string { return c.Value("UID") }

// FN returns the formatted name.
func (c Card) FN() string { return c.Value("FN") }

// Kind returns the vCard 4.0 KIND, also reading Apple's
// X-ADDRESSBOOKSERVER-KIND. The default is "individual".
func (c Card) Kind() string {
	if k := c.Value("KIND"); k != "" {
		return strings.ToLower(k)
	}

	if k := c.Value("X-ADDRESSBOOKSERVER-KIND"); k != "" {
		return strings.ToLower(k)
	}

	return "individual"
}

// Label returns the X-ABLabel attached to a grouped line, with Apple's
// _$!<...>!$_ wrapper removed.
func (c Card) Label(l vcf.Line) string {
	if l.Group == "" {
		return ""
	}

	for _, other := range c.Lines {
		if other.Group == l.Group && other.Name == "X-ABLABEL" {
			return AppleLabel(other.Text())
		}
	}

	return ""
}

// AppleLabel strips the _$!<Label>!$_ wrapper that Apple uses for its
// built-in labels.
func AppleLabel(s string) string {
	if strings.HasPrefix(s, "_$!<") && strings.HasSuffix(s, ">!$_") {
		return s[4 : len(s)-4]
	}

	return s
}

// Set replaces the value of the first property with that name, or appends
// one before END:VCARD.
func (c *Card) Set(name, value string) {
	for i := range c.Lines {
		if c.Lines[i].Name == name {
			c.Lines[i].Value = value

			return
		}
	}

	c.insertBeforeEnd(vcf.Line{Group: "", Name: name, Params: nil, Value: value, Spelling: ""})
}

func (c *Card) insertBeforeEnd(l vcf.Line) {
	end := len(c.Lines)
	if end > 0 && c.Lines[end-1].Name == "END" {
		end--
	}

	c.Lines = append(c.Lines[:end], append([]vcf.Line{l}, c.Lines[end:]...)...)
}

// Clone returns a deep copy.
func (c Card) Clone() Card {
	out := Card{Lines: make([]vcf.Line, len(c.Lines))}

	for i, l := range c.Lines {
		params := make([]vcf.Param, len(l.Params))
		for j, p := range l.Params {
			params[j] = vcf.Param{Name: p.Name, Values: append([]string(nil), p.Values...), Spelling: p.Spelling}
		}

		l.Params = params
		out.Lines[i] = l
	}

	return out
}
