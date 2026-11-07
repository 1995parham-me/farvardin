package calendar_test

import (
	"testing"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/calendar"
	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

//nolint:gosmopolitan // it sets time.Local, so it does not run in parallel
func TestParseTimeZones(t *testing.T) {
	madrid, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Skip("no time zone database")
	}

	saved := time.Local
	time.Local = madrid

	t.Cleanup(func() { time.Local = saved })

	cases := []struct {
		line string
		want string // RFC 3339 in UTC
		day  bool
	}{
		{"DTSTART:20261110T090000Z", "2026-11-10T09:00:00Z", false},
		{"DTSTART;TZID=Asia/Tehran:20261110T090000", "2026-11-10T05:30:00Z", false},
		{`DTSTART;TZID="Mountain Standard Time":20261110T090000`, "2026-11-10T16:00:00Z", false},
		{"DTSTART;TZID=Romance Standard Time:20260710T090000", "2026-07-10T07:00:00Z", false},
		// Floating: nine o'clock where the owner is.
		{"DTSTART:20261110T090000", "2026-11-10T08:00:00Z", false},
		// A day starts at midnight where the owner is.
		{"DTSTART;VALUE=DATE:20261110", "2026-11-09T23:00:00Z", true},
		// A zone nothing knows is read like a floating time.
		{"DTSTART;TZID=Somewhere/Else:20261110T090000", "2026-11-10T08:00:00Z", false},
	}

	for _, c := range cases {
		l, err := vcf.ParseLine(c.line)
		if err != nil {
			t.Fatal(err)
		}

		got, day, err := calendar.ParseTime(l)
		if err != nil || day != c.day || got.UTC().Format(time.RFC3339) != c.want {
			t.Errorf("%s = %s %v %v; want %s %v", c.line, got.UTC().Format(time.RFC3339), day, err, c.want, c.day)
		}
	}
}
