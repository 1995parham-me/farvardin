package calendar_test

import (
	"strings"
	"testing"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/calendar"
	"github.com/1995parham-me/farvardin/internal/domain/contact"
)

const weekly = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:t\r\n" +
	"BEGIN:VTIMEZONE\r\nTZID:Asia/Tehran\r\nEND:VTIMEZONE\r\n" +
	"BEGIN:VEVENT\r\nUID:w\r\nDTSTART;TZID=Asia/Tehran:20260105T090000\r\nDURATION:PT1H\r\n" +
	"RRULE:FREQ=WEEKLY;UNTIL=20260301T000000Z\r\nSUMMARY:Standup\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func mustParse(t *testing.T, s string) *calendar.Component {
	t.Helper()

	c, err := calendar.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func date(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)

	return t
}

func TestOverlapsRecurring(t *testing.T) {
	t.Parallel()

	c := mustParse(t, weekly)

	if !calendar.Overlaps(c, date("2026-02-01"), date("2026-02-08")) {
		t.Fatal("a weekly series must match a week inside it")
	}

	if calendar.Overlaps(c, date("2026-06-01"), date("2026-06-08")) {
		t.Fatal("a series that ended in March must not match June")
	}

	if calendar.Overlaps(c, date("2025-01-01"), date("2025-02-01")) {
		t.Fatal("a series must not match before it starts")
	}

	next, ok := calendar.NextOccurrence(c, date("2026-01-20"))
	if !ok || next.Format("2006-01-02") != "2026-01-26" {
		t.Fatalf("next occurrence %s %v", next, ok)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	uid, err := calendar.Validate(mustParse(t, weekly), []string{"VEVENT"})
	if err != nil || uid != "w" {
		t.Fatalf("uid %q err %v", uid, err)
	}

	if _, err := calendar.Validate(mustParse(t, weekly), []string{"VTODO"}); err == nil {
		t.Fatal("VEVENT in a VTODO-only calendar must fail")
	}

	two := strings.Replace(weekly, "END:VCALENDAR", "BEGIN:VEVENT\r\nUID:other\r\nDTSTART:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR", 1)
	if _, err := calendar.Validate(mustParse(t, two), nil); err == nil {
		t.Fatal("two UIDs in one resource must fail")
	}
}

func TestDuration(t *testing.T) {
	t.Parallel()

	d, err := calendar.ParseDuration("-P1W2DT3H")
	if err != nil || d != -(9*24+3)*time.Hour {
		t.Fatalf("duration %v %v", d, err)
	}
}

func TestBirthdays(t *testing.T) {
	t.Parallel()

	card, err := contact.Parse([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:x\r\nFN:Leap\r\nBDAY:--0229\r\nANNIVERSARY:20200101\r\nEND:VCARD\r\n"))
	if err != nil {
		t.Fatal(err)
	}

	events := calendar.Birthdays("x.vcf", card, calendar.BirthdayOptions{AlarmDays: 1, ProductID: ""})
	if len(events) != 2 || events[0].Name != "x-birthday.ics" || events[1].Name != "x-anniversary.ics" {
		t.Fatalf("events: %+v", events)
	}

	body := string(events[0].Data)
	for _, want := range []string{"DTSTART;VALUE=DATE:19720229", "BYMONTHDAY=-1", "TRIGGER:-P1D", "SUMMARY:🎂 Leap"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}

	// Generation is deterministic, so ETags only change with the card.
	again := calendar.Birthdays("x.vcf", card, calendar.BirthdayOptions{AlarmDays: 1, ProductID: ""})
	if string(again[0].Data) != body {
		t.Fatal("birthday events are not deterministic")
	}

	if _, err := calendar.Parse(events[0].Data); err != nil {
		t.Fatalf("generated event does not parse: %v", err)
	}
}
