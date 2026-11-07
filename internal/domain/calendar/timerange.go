package calendar

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// ErrBadTime is returned for a date or date-time that cannot be read.
var ErrBadTime = errors.New("bad iCalendar date or date-time")

// ParseTime reads a DATE or DATE-TIME property. A time with "Z" is UTC;
// one with a TZID is read in that zone when the Go time zone database
// knows it, directly or as a Windows zone name ("Mountain Standard
// Time"). A floating time (neither) and a DATE are the same wall-clock
// time wherever the reader is (RFC 5545 3.3.5), so they are read in
// time.Local, the zone farvardin shows times in; so is a TZID nothing
// knows (callers that match ranges widen them to cover the error).
func ParseTime(p vcf.Line) (time.Time, bool, error) {
	v := strings.TrimSpace(p.Value)
	if i := strings.IndexByte(v, ','); i > 0 {
		v = v[:i] // RDATE / EXDATE lists: the first entry is enough here
	}

	if len(v) == 8 || strings.EqualFold(firstValue(p.Param("VALUE")), "DATE") {
		t, err := time.ParseInLocation("20060102", v[:min(8, len(v))], time.Local) //nolint:gosmopolitan // a DATE is a day where the reader is
		if err != nil {
			return time.Time{}, true, ErrBadTime
		}

		return t, true, nil
	}

	loc := time.Local //nolint:gosmopolitan // a floating time is wall-clock time where the reader is
	if trimmed, ok := strings.CutSuffix(v, "Z"); ok {
		v, loc = trimmed, time.UTC
	} else if tz := firstValue(p.Param("TZID")); tz != "" {
		if l, ok := Zone(tz); ok {
			loc = l
		}
	}

	t, err := time.ParseInLocation("20060102T150405", v, loc)
	if err != nil {
		return time.Time{}, false, ErrBadTime
	}

	return t, false, nil
}

func firstValue(v []string) string {
	if len(v) == 0 {
		return ""
	}

	return v[0]
}

// end returns DTEND, DUE, DTSTART+DURATION, or the RFC 5545 default
// (one day for all-day events, the start itself otherwise).
func end(c *Component, start time.Time, allDay bool) time.Time {
	for _, name := range []string{"DTEND", "DUE"} {
		if p, ok := c.Get(name); ok {
			if t, _, err := ParseTime(p); err == nil {
				return t
			}
		}
	}

	if p, ok := c.Get("DURATION"); ok {
		if d, err := ParseDuration(p.Value); err == nil {
			return start.Add(d)
		}
	}

	if allDay {
		return start.AddDate(0, 0, 1)
	}

	return start
}

// ParseDuration reads an RFC 5545 duration such as P1D, PT1H30M or -P2W.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	negative := false

	switch {
	case strings.HasPrefix(s, "-"):
		negative, s = true, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}

	if !strings.HasPrefix(s, "P") {
		return 0, ErrBadTime
	}

	s = s[1:]

	var (
		total  time.Duration
		number strings.Builder
	)

	units := map[byte]time.Duration{'W': 7 * 24 * time.Hour, 'D': 24 * time.Hour, 'H': time.Hour, 'M': time.Minute, 'S': time.Second}

	for i := range len(s) {
		ch := s[i]

		switch {
		case ch == 'T':
		case ch >= '0' && ch <= '9':
			number.WriteByte(ch)
		default:
			unit, ok := units[ch]
			if !ok || number.Len() == 0 {
				return 0, ErrBadTime
			}

			n, err := strconv.Atoi(number.String())
			if err != nil {
				return 0, ErrBadTime
			}

			total += time.Duration(n) * unit

			number.Reset()
		}
	}

	if negative {
		total = -total
	}

	return total, nil
}

// slack covers times whose zone could not be resolved (read as UTC).
const slack = 14 * time.Hour

// Overlaps reports whether any main component of cal may fall inside
// [start, end). Zero times mean an open range. For repeating components
// the answer errs on the side of true (a superset), which RFC 4791 clients
// accept: they filter again on their side.
func Overlaps(cal *Component, start, endT time.Time) bool {
	for _, m := range cal.Main() {
		if componentOverlaps(m, start, endT) {
			return true
		}
	}

	return false
}

func componentOverlaps(m *Component, start, endT time.Time) bool {
	p, ok := m.Get("DTSTART")
	if !ok {
		p, ok = m.Get("DUE")
	}

	if !ok {
		return true // a task with no dates matches every range
	}

	s, allDay, err := ParseTime(p)
	if err != nil {
		return true
	}

	e := end(m, s, allDay)
	if e.Equal(s) {
		e = s.Add(time.Second)
	}

	_, rrule := m.Get("RRULE")
	_, rdate := m.Get("RDATE")

	if rrule || rdate {
		if !endT.IsZero() && s.After(endT.Add(slack)) {
			return false
		}

		if r, ok := m.Get("RRULE"); ok && !start.IsZero() {
			if until := ruleUntil(r.Value); !until.IsZero() && until.Before(start.Add(-slack)) && !rdate {
				return false
			}
		}

		return true
	}

	if !endT.IsZero() && !s.Before(endT.Add(slack)) {
		return false
	}

	if !start.IsZero() && !e.After(start.Add(-slack)) {
		return false
	}

	return true
}

func ruleUntil(rule string) time.Time {
	for part := range strings.SplitSeq(rule, ";") {
		if v, ok := strings.CutPrefix(strings.ToUpper(part), "UNTIL="); ok {
			t, _, err := ParseTime(vcf.Line{Group: "", Name: "UNTIL", Params: nil, Value: v, Spelling: ""})
			if err == nil {
				return t
			}
		}
	}

	return time.Time{}
}

// NextOccurrence returns the first start on or after from for a summary
// whose master component has a simple RRULE (FREQ and INTERVAL only). For
// other rules it returns the series start and false.
func NextOccurrence(cal *Component, from time.Time) (time.Time, bool) {
	s, ok := Summarize(cal)
	if !ok {
		return time.Time{}, false
	}

	if !s.Repeats {
		return s.Start, true
	}

	var rule string

	for _, m := range cal.Main() {
		if r, ok := m.Get("RRULE"); ok {
			rule = r.Value
		}
	}

	freq, interval := "", 1

	for part := range strings.SplitSeq(strings.ToUpper(rule), ";") {
		key, val, _ := strings.Cut(part, "=")

		switch key {
		case "FREQ":
			freq = val
		case "INTERVAL":
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				interval = n
			}
		case "UNTIL", "COUNT", "WKST":
		default:
			return s.Start, false
		}
	}

	t := s.Start

	for i := 0; t.Before(from) && i < 100000; i++ {
		switch freq {
		case "YEARLY":
			t = t.AddDate(interval, 0, 0)
		case "MONTHLY":
			t = t.AddDate(0, interval, 0)
		case "WEEKLY":
			t = t.AddDate(0, 0, 7*interval)
		case "DAILY":
			t = t.AddDate(0, 0, interval)
		default:
			return s.Start, false
		}
	}

	if until := ruleUntil(rule); !until.IsZero() && t.After(until) {
		return s.Start, false
	}

	return t, true
}

// windowsZones maps the zone names Outlook and Exchange write in TZID to
// IANA names (from CLDR's windowsZones.xml, the zones most likely to turn
// up).
var windowsZones = map[string]string{
	"Dateline Standard Time": "Etc/GMT+12", "Hawaiian Standard Time": "Pacific/Honolulu",
	"Alaskan Standard Time": "America/Anchorage", "Pacific Standard Time": "America/Los_Angeles",
	"US Mountain Standard Time": "America/Phoenix", "Mountain Standard Time": "America/Denver",
	"Central Standard Time": "America/Chicago", "Eastern Standard Time": "America/New_York",
	"Atlantic Standard Time": "America/Halifax", "Newfoundland Standard Time": "America/St_Johns",
	"E. South America Standard Time": "America/Sao_Paulo", "UTC": "Etc/UTC", "Coordinated Universal Time": "Etc/UTC",
	"GMT Standard Time": "Europe/London", "Greenwich Standard Time": "Atlantic/Reykjavik",
	"W. Europe Standard Time": "Europe/Berlin", "Romance Standard Time": "Europe/Paris",
	"Central Europe Standard Time": "Europe/Budapest", "Central European Standard Time": "Europe/Warsaw",
	"GTB Standard Time": "Europe/Bucharest", "FLE Standard Time": "Europe/Kiev", "E. Europe Standard Time": "Europe/Chisinau",
	"Turkey Standard Time": "Europe/Istanbul", "Russian Standard Time": "Europe/Moscow",
	"Israel Standard Time": "Asia/Jerusalem", "Arabian Standard Time": "Asia/Dubai", "Arab Standard Time": "Asia/Riyadh",
	"Iran Standard Time": "Asia/Tehran", "Afghanistan Standard Time": "Asia/Kabul", "Pakistan Standard Time": "Asia/Karachi",
	"India Standard Time": "Asia/Calcutta", "China Standard Time": "Asia/Shanghai", "Singapore Standard Time": "Asia/Singapore",
	"Tokyo Standard Time": "Asia/Tokyo", "Korea Standard Time": "Asia/Seoul",
	"AUS Eastern Standard Time": "Australia/Sydney", "E. Australia Standard Time": "Australia/Brisbane",
	"New Zealand Standard Time": "Pacific/Auckland",
}

// Zone returns the location a TZID names: an IANA name, or a Windows
// zone name Outlook writes.
func Zone(tzid string) (*time.Location, bool) {
	tzid = strings.Trim(strings.TrimPrefix(strings.TrimSpace(tzid), "/"), `"`)

	if l, err := time.LoadLocation(tzid); err == nil && tzid != "" && tzid != "Local" {
		return l, true
	}

	if name, ok := windowsZones[tzid]; ok {
		if l, err := time.LoadLocation(name); err == nil {
			return l, true
		}
	}

	return nil, false
}
