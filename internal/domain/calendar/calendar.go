// Package calendar models iCalendar objects (RFC 5545): it parses them
// into a component tree, validates them for CalDAV (RFC 4791), matches
// them against time ranges, and builds the birthdays calendar.
package calendar

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// Errors returned by Parse and Validate.
var (
	ErrNoCalendar   = errors.New("no VCALENDAR found")
	ErrUnbalanced   = errors.New("BEGIN and END do not match")
	ErrNoComponent  = errors.New("VCALENDAR has no VEVENT, VTODO or VJOURNAL")
	ErrMixedUIDs    = errors.New("one resource holds more than one UID")
	ErrMissingUID   = errors.New("component has no UID")
	ErrMixedTypes   = errors.New("one resource holds more than one component type")
	ErrNotSupported = errors.New("component type is not supported by this calendar")
)

// Component is a BEGIN/END block with its properties and children.
type Component struct {
	Name       string
	Props      []vcf.Line
	Components []*Component
}

// Parse reads one iCalendar stream.
func Parse(data []byte) (*Component, error) {
	lines, err := vcf.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing iCalendar: %w", err)
	}

	var (
		stack []*Component
		root  *Component
	)

	for _, l := range lines {
		switch l.Name {
		case "BEGIN":
			c := &Component{Name: strings.ToUpper(l.Value), Props: nil, Components: nil}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.Components = append(parent.Components, c)
			} else if root == nil {
				root = c
			}

			stack = append(stack, c)
		case "END":
			if len(stack) == 0 || stack[len(stack)-1].Name != strings.ToUpper(l.Value) {
				return nil, ErrUnbalanced
			}

			stack = stack[:len(stack)-1]
		default:
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				top.Props = append(top.Props, l)
			}
		}
	}

	if len(stack) != 0 {
		return nil, ErrUnbalanced
	}

	if root == nil || root.Name != "VCALENDAR" {
		return nil, ErrNoCalendar
	}

	return root, nil
}

// Get returns the first property with that name.
func (c *Component) Get(name string) (vcf.Line, bool) {
	for _, p := range c.Props {
		if p.Name == name {
			return p, true
		}
	}

	return vcf.Line{}, false
}

// Value returns the unescaped text of the first property with that name.
func (c *Component) Value(name string) string {
	p, ok := c.Get(name)
	if !ok {
		return ""
	}

	return p.Text()
}

// Children returns the child components with that name.
func (c *Component) Children(name string) []*Component {
	var out []*Component

	for _, child := range c.Components {
		if child.Name == name {
			out = append(out, child)
		}
	}

	return out
}

// Main returns the scheduling components (VEVENT, VTODO, VJOURNAL,
// VFREEBUSY, VAVAILABILITY) of a VCALENDAR.
func (c *Component) Main() []*Component {
	var out []*Component

	for _, child := range c.Components {
		switch child.Name {
		case "VEVENT", "VTODO", "VJOURNAL", "VFREEBUSY", "VAVAILABILITY":
			out = append(out, child)
		}
	}

	return out
}

// Encode writes the component tree.
func (c *Component) Encode() []byte {
	var lines []vcf.Line

	var walk func(*Component)

	walk = func(c *Component) {
		lines = append(lines, vcf.Line{Group: "", Name: "BEGIN", Params: nil, Value: c.Name, Spelling: ""})
		lines = append(lines, c.Props...)

		for _, child := range c.Components {
			walk(child)
		}

		lines = append(lines, vcf.Line{Group: "", Name: "END", Params: nil, Value: c.Name, Spelling: ""})
	}

	walk(c)

	return vcf.Encode(lines)
}

// Validate checks the CalDAV rules of RFC 4791 section 4.1: one component
// type and one UID per resource. supported lists the component names the
// calendar accepts; an empty list accepts all.
func Validate(cal *Component, supported []string) (string, error) {
	main := cal.Main()
	if len(main) == 0 {
		return "", ErrNoComponent
	}

	uid, kind := "", ""

	for _, m := range main {
		u := m.Value("UID")
		if u == "" {
			return "", ErrMissingUID
		}

		if uid != "" && u != uid {
			return "", ErrMixedUIDs
		}

		if kind != "" && m.Name != kind {
			return "", ErrMixedTypes
		}

		uid, kind = u, m.Name
	}

	if len(supported) > 0 {
		ok := false

		for _, s := range supported {
			if strings.EqualFold(s, kind) {
				ok = true
			}
		}

		if !ok {
			return "", fmt.Errorf("%w: %s", ErrNotSupported, kind)
		}
	}

	return uid, nil
}

// Summary describes one event or task for listings.
type Summary struct {
	UID      string
	Kind     string
	Title    string
	Location string
	Start    time.Time
	End      time.Time
	AllDay   bool
	Repeats  bool
	Status   string
}

// Summarize returns the first main component of the calendar as a Summary.
func Summarize(cal *Component) (Summary, bool) {
	main := cal.Main()
	if len(main) == 0 {
		return Summary{}, false
	}

	// The master component is the one without RECURRENCE-ID.
	m := main[0]

	for _, c := range main {
		if _, ok := c.Get("RECURRENCE-ID"); !ok {
			m = c

			break
		}
	}

	s := Summary{
		UID: m.Value("UID"), Kind: m.Name, Title: m.Value("SUMMARY"),
		Location: m.Value("LOCATION"), Status: m.Value("STATUS"),
		Start: time.Time{}, End: time.Time{}, AllDay: false, Repeats: false,
	}

	if p, ok := m.Get("DTSTART"); ok {
		s.Start, s.AllDay, _ = ParseTime(p)
	} else if p, ok := m.Get("DUE"); ok {
		s.Start, s.AllDay, _ = ParseTime(p)
	}

	s.End = end(m, s.Start, s.AllDay)
	_, rrule := m.Get("RRULE")
	_, rdate := m.Get("RDATE")
	s.Repeats = rrule || rdate

	return s, true
}
