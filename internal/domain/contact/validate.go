package contact

import (
	"fmt"
	"strings"
)

// Severity of a validation issue.
type Severity string

// Severities. An Error blocks a write; a Warning is only reported.
const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Issue is one problem found in a card.
type Issue struct {
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	// Fix is the value the problem's property should have, when it can
	// be fixed without asking (a phone number in E.164).
	Fix string `json:"fix,omitempty"`
}

func (i Issue) String() string {
	if i.Fix != "" {
		return string(i.Severity) + ": " + i.Message + " (fix: " + i.Fix + ")"
	}

	return string(i.Severity) + ": " + i.Message
}

// MaxPhotoBytes is the size above which a photo is reported.
const MaxPhotoBytes = 1 << 20

// Validate checks a card against RFC 6350 / RFC 2426 and the needs of
// CardDAV (RFC 6352: one card per resource, with a UID).
func Validate(c Card) []Issue {
	var issues []Issue

	add := func(s Severity, format string, args ...any) {
		issues = append(issues, Issue{Severity: s, Message: fmt.Sprintf(format, args...), Fix: ""})
	}

	switch v := c.Version(); v {
	case "3.0", "4.0":
	case "":
		add(Error, "VERSION is missing")
	default:
		add(Error, "VERSION %q is not supported (only 3.0 and 4.0)", v)
	}

	if len(c.All("VERSION")) > 1 {
		add(Error, "VERSION appears more than once")
	}

	if c.UID() == "" {
		add(Error, "UID is missing (RFC 6352 requires one)")
	}

	if _, ok := c.Get("FN"); !ok {
		add(Error, "FN is missing")
	} else if strings.TrimSpace(c.FN()) == "" {
		add(Warning, "FN is empty")
	}

	if _, ok := c.Get("N"); !ok && c.Version() == "3.0" && c.Kind() == "individual" {
		add(Warning, "N is missing (vCard 3.0 requires it)")
	}

	for _, d := range []string{"BDAY", "ANNIVERSARY"} {
		if l, ok := c.Get(d); ok {
			if _, ok := ParseDate(l.Value); !ok && !strings.EqualFold(firstParam(l.Param("VALUE")), "text") {
				add(Warning, "%s %q is not a date", d, l.Value)
			}
		}
	}

	for _, l := range c.All("EMAIL") {
		if v := l.Text(); !strings.Contains(v, "@") {
			add(Warning, "EMAIL %q has no @", v)
		}
	}

	for _, l := range c.All("PHOTO") {
		if n := len(l.Value); n*3/4 > MaxPhotoBytes {
			add(Warning, "PHOTO is %d KiB", n*3/4/1024)
		}
	}

	if len(c.All("PHOTO")) > 1 {
		add(Warning, "more than one PHOTO")
	}

	return issues
}

// HasErrors reports whether any issue is an Error.
func HasErrors(issues []Issue) bool {
	for _, i := range issues {
		if i.Severity == Error {
			return true
		}
	}

	return false
}

func firstParam(v []string) string {
	if len(v) == 0 {
		return ""
	}

	return v[0]
}
