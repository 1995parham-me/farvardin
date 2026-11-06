package calendar

import (
	"strings"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// EventFields are the parts of an event that can be set.
type EventFields struct {
	Summary     *string
	Description *string
	Location    *string
	Start       *time.Time
	End         *time.Time
	// AllDay makes Start/End dates; End is exclusive (the day after).
	AllDay *bool
	// RRule is an RFC 5545 rule such as "FREQ=WEEKLY;BYDAY=MO"; "" removes it.
	RRule *string
}

// NewEvent returns a VCALENDAR with one VEVENT.
func NewEvent(uid string, now time.Time, f EventFields) *Component {
	ev := &Component{Name: "VEVENT", Props: []vcf.Line{
		prop("UID", uid),
		prop("DTSTAMP", now.UTC().Format("20060102T150405Z")),
	}, Components: nil}
	ApplyEvent(ev, now, f)

	return &Component{
		Name:       "VCALENDAR",
		Props:      []vcf.Line{prop("VERSION", "2.0"), prop("PRODID", "-//farvardin//EN")},
		Components: []*Component{ev},
	}
}

func prop(name, value string) vcf.Line {
	return vcf.Line{Group: "", Name: name, Params: nil, Value: value, Spelling: ""}
}

// ApplyEvent changes the set fields of a VEVENT and bumps SEQUENCE and
// LAST-MODIFIED, so clients see a new version.
func ApplyEvent(ev *Component, now time.Time, f EventFields) {
	set := func(name, value string) {
		props := ev.Props[:0]

		for _, p := range ev.Props {
			if p.Name != name {
				props = append(props, p)
			}
		}

		ev.Props = props
		if value != "" {
			ev.Props = append(ev.Props, prop(name, value))
		}
	}

	if f.Summary != nil {
		set("SUMMARY", vcf.Escape(*f.Summary))
	}

	if f.Description != nil {
		set("DESCRIPTION", vcf.Escape(*f.Description))
	}

	if f.Location != nil {
		set("LOCATION", vcf.Escape(*f.Location))
	}

	if f.RRule != nil {
		set("RRULE", strings.ToUpper(strings.TrimSpace(*f.RRule)))
	}

	allDay := false
	if p, ok := ev.Get("DTSTART"); ok {
		_, allDay, _ = ParseTime(p)
	}

	if f.AllDay != nil {
		allDay = *f.AllDay
	}

	when := func(name string, t time.Time) {
		set(name, "")

		if allDay {
			ev.Props = append(ev.Props, vcf.Line{Group: "", Name: name, Params: []vcf.Param{{Name: "VALUE", Values: []string{"DATE"}, Spelling: ""}}, Value: t.Format("20060102"), Spelling: ""})
		} else {
			ev.Props = append(ev.Props, prop(name, t.UTC().Format("20060102T150405Z")))
		}
	}

	if f.Start != nil {
		when("DTSTART", *f.Start)
		set("DURATION", "")
	}

	if f.End != nil {
		when("DTEND", *f.End)
		set("DURATION", "")
	}

	seq := 0
	if p, ok := ev.Get("SEQUENCE"); ok {
		for _, ch := range p.Value {
			if ch >= '0' && ch <= '9' {
				seq = seq*10 + int(ch-'0')
			}
		}

		seq++
	}

	set("SEQUENCE", itoa(seq))
	set("LAST-MODIFIED", now.UTC().Format("20060102T150405Z"))
	set("DTSTAMP", now.UTC().Format("20060102T150405Z"))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var b []byte

	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}

	return string(b)
}

// Master returns the component without RECURRENCE-ID (the series), or the
// first one.
func Master(cal *Component) *Component {
	main := cal.Main()
	if len(main) == 0 {
		return nil
	}

	for _, c := range main {
		if _, ok := c.Get("RECURRENCE-ID"); !ok {
			return c
		}
	}

	return main[0]
}
