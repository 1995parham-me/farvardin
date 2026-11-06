package calendar

import (
	"fmt"
	"strings"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// BirthdayEvent is one generated yearly event.
type BirthdayEvent struct {
	// Name is the resource name inside the birthdays collection.
	Name string
	Data []byte
}

// BirthdayOptions controls the generated events.
type BirthdayOptions struct {
	// AlarmDays adds a reminder that many days before; 0 adds none.
	AlarmDays int
	// ProductID is written as PRODID.
	ProductID string
}

// Birthdays builds one yearly all-day event per birthday and anniversary
// of the card. resource is the card's file name, used to make stable
// event names and UIDs, so the calendar does not change unless the cards
// do.
func Birthdays(resource string, c contact.Card, opts BirthdayOptions) []BirthdayEvent {
	stem := strings.TrimSuffix(resource, ".vcf")

	var out []BirthdayEvent

	seen := map[string]int{}

	for _, d := range c.Dates() {
		if d.Kind == contact.OtherDate {
			continue
		}

		suffix := "birthday"
		emoji := "🎂"

		if d.Kind == contact.Anniversary {
			suffix, emoji = "anniversary", "💍"
		}

		category := suffix
		if seen[category]++; seen[category] > 1 {
			suffix = fmt.Sprintf("%s-%d", suffix, seen[category])
		}

		year := d.Year
		if year == 0 {
			// RFC 5545 needs a year; 1972 is a leap year, so 29 February works.
			year = 1972
		}

		title := emoji + " " + c.DisplayName()
		if d.Year != 0 && d.Kind == contact.Birthday {
			title += fmt.Sprintf(" (%d)", d.Year)
		}

		start := fmt.Sprintf("%04d%02d%02d", year, int(d.Month), d.Day)
		uid := stem + "-" + suffix + "@farvardin"

		props := []vcf.Line{
			line("UID", uid),
			line("DTSTAMP", "20000101T000000Z"),
			{Group: "", Name: "DTSTART", Params: []vcf.Param{{Name: "VALUE", Values: []string{"DATE"}, Spelling: ""}}, Value: start, Spelling: ""},
			line("DURATION", "P1D"),
			line("RRULE", rrule(d)),
			line("SUMMARY", vcf.Escape(title)),
			line("TRANSP", "TRANSPARENT"),
			line("CATEGORIES", category),
			line("X-FARVARDIN-CONTACT", c.UID()),
		}

		event := &Component{Name: "VEVENT", Props: props, Components: nil}

		if opts.AlarmDays > 0 {
			event.Components = append(event.Components, &Component{
				Name: "VALARM",
				Props: []vcf.Line{
					line("ACTION", "DISPLAY"),
					line("DESCRIPTION", vcf.Escape(title)),
					line("TRIGGER", fmt.Sprintf("-P%dD", opts.AlarmDays)),
				},
				Components: nil,
			})
		}

		prodID := opts.ProductID
		if prodID == "" {
			prodID = "-//farvardin//birthdays//EN"
		}

		cal := &Component{
			Name:       "VCALENDAR",
			Props:      []vcf.Line{line("VERSION", "2.0"), line("PRODID", prodID)},
			Components: []*Component{event},
		}

		out = append(out, BirthdayEvent{Name: stem + "-" + suffix + ".ics", Data: cal.Encode()})
	}

	return out
}

func rrule(d contact.Date) string {
	if d.Month == 2 && d.Day == 29 {
		// Fall back to the last day of February in common years.
		return "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=-1"
	}

	return "FREQ=YEARLY"
}

func line(name, value string) vcf.Line {
	return vcf.Line{Group: "", Name: name, Params: nil, Value: value, Spelling: ""}
}
