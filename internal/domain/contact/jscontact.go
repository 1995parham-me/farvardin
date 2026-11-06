package contact

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// JSContact is a Card object of RFC 9553, built with the vCard mapping of
// RFC 9555. Only the properties a contact usually has are mapped; the rest
// of the vCard is kept under vCardProps so nothing is lost.
type JSContact struct {
	Type          string                     `json:"@type"`
	Version       string                     `json:"version"`
	UID           string                     `json:"uid"`
	Kind          string                     `json:"kind,omitempty"`
	Updated       string                     `json:"updated,omitempty"`
	Name          *JSName                    `json:"name,omitempty"`
	Nicknames     map[string]JSNickname      `json:"nicknames,omitempty"`
	Organizations map[string]JSOrganization  `json:"organizations,omitempty"`
	Titles        map[string]JSTitle         `json:"titles,omitempty"`
	Emails        map[string]JSEmail         `json:"emails,omitempty"`
	Phones        map[string]JSPhone         `json:"phones,omitempty"`
	Addresses     map[string]JSAddress       `json:"addresses,omitempty"`
	Links         map[string]JSLink          `json:"links,omitempty"`
	OnlineSvcs    map[string]JSOnlineService `json:"onlineServices,omitempty"`
	Anniversaries map[string]JSAnniversary   `json:"anniversaries,omitempty"`
	Notes         map[string]JSNote          `json:"notes,omitempty"`
	Media         map[string]JSMedia         `json:"media,omitempty"`
	SpeakToAs     *JSSpeakToAs               `json:"speakToAs,omitempty"`
	Members       map[string]bool            `json:"members,omitempty"`
	Keywords      map[string]bool            `json:"keywords,omitempty"`
	VCardProps    [][]any                    `json:"vCardProps,omitempty"`
}

// JSName is the RFC 9553 Name object.
type JSName struct {
	Components []JSNameComponent `json:"components,omitempty"`
	Full       string            `json:"full,omitempty"`
}

// JSNameComponent is one part of a name.
type JSNameComponent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// JSNickname is a nickname.
type JSNickname struct {
	Name string `json:"name"`
}

// JSOrganization is an organization with its units.
type JSOrganization struct {
	Name  string           `json:"name,omitempty"`
	Units []map[string]any `json:"units,omitempty"`
}

// JSTitle is a job title or role.
type JSTitle struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// JSEmail is an email address.
type JSEmail struct {
	Address  string          `json:"address"`
	Contexts map[string]bool `json:"contexts,omitempty"`
	Pref     int             `json:"pref,omitempty"`
	Label    string          `json:"label,omitempty"`
}

// JSPhone is a phone number.
type JSPhone struct {
	Number   string          `json:"number"`
	Features map[string]bool `json:"features,omitempty"`
	Contexts map[string]bool `json:"contexts,omitempty"`
	Pref     int             `json:"pref,omitempty"`
	Label    string          `json:"label,omitempty"`
}

// JSAddress is a postal address.
type JSAddress struct {
	Full     string          `json:"full,omitempty"`
	Contexts map[string]bool `json:"contexts,omitempty"`
	Pref     int             `json:"pref,omitempty"`
}

// JSLink is a link.
type JSLink struct {
	URI   string `json:"uri"`
	Label string `json:"label,omitempty"`
}

// JSOnlineService is an online account (RFC 9554 SOCIALPROFILE / IMPP).
type JSOnlineService struct {
	Service string `json:"service,omitempty"`
	URI     string `json:"uri,omitempty"`
	User    string `json:"user,omitempty"`
}

// JSAnniversary is a birthday or anniversary.
type JSAnniversary struct {
	Kind  string      `json:"kind"`
	Date  PartialDate `json:"date"`
	Label string      `json:"label,omitempty"`
}

// PartialDate is the RFC 9553 PartialDate object.
type PartialDate struct {
	Type  string `json:"@type"`
	Year  int    `json:"year,omitempty"`
	Month int    `json:"month"`
	Day   int    `json:"day"`
}

// JSNote is a free-text note.
type JSNote struct {
	Note string `json:"note"`
}

// JSMedia is a photo, logo or sound.
type JSMedia struct {
	Kind      string `json:"kind"`
	URI       string `json:"uri"`
	MediaType string `json:"mediaType,omitempty"`
}

// JSSpeakToAs carries pronouns and grammatical gender (RFC 9554).
type JSSpeakToAs struct {
	GrammaticalGender string               `json:"grammaticalGender,omitempty"`
	Pronouns          map[string]JSPronoun `json:"pronouns,omitempty"`
}

// JSPronoun is one set of pronouns.
type JSPronoun struct {
	Pronouns string `json:"pronouns"`
}

// handled are the vCard properties mapped to JSContact fields; every other
// property goes to vCardProps.
var handled = map[string]bool{
	"BEGIN": true, "END": true, "VERSION": true, "PRODID": true, "UID": true,
	"KIND": true, "X-ADDRESSBOOKSERVER-KIND": true, "REV": true, "N": true,
	"FN": true, "NICKNAME": true, "ORG": true, "TITLE": true, "ROLE": true,
	"EMAIL": true, "TEL": true, "ADR": true, "URL": true, "SOCIALPROFILE": true,
	"X-SOCIALPROFILE": true, "IMPP": true, "BDAY": true, "ANNIVERSARY": true,
	"X-ANNIVERSARY": true, "NOTE": true, "PHOTO": true, "LOGO": true,
	"PRONOUNS": true, "GRAMGENDER": true, "MEMBER": true,
	"X-ADDRESSBOOKSERVER-MEMBER": true, "CATEGORIES": true, "X-ABLABEL": true,
	"X-ABDATE": true,
}

// ToJSContact converts a card to JSContact. With photos false, inline
// photos are left out (they can be large).
//
//nolint:gocognit,gocyclo,cyclop,funlen // one flat switch over the vCard properties reads better split up than not
func ToJSContact(c Card, photos bool) JSContact {
	js := JSContact{Type: "Card", Version: "1.0", UID: c.UID(), Kind: "", Updated: "", Name: nil}
	counters := map[string]int{}

	id := func(prefix string) string {
		counters[prefix]++

		return prefix + strconv.Itoa(counters[prefix])
	}

	if k := c.Kind(); k != "individual" {
		js.Kind = k
	}

	if rev := c.Value("REV"); rev != "" {
		js.Updated = rev
	}

	if _, ok := c.Get("N"); ok || c.FN() != "" {
		n := c.Name()
		js.Name = &JSName{Full: c.FN(), Components: nil}

		for _, part := range []struct{ kind, value string }{
			{"title", n.Prefix}, {"given", n.Given}, {"given2", n.Additional},
			{"surname", n.Family}, {"credential", n.Suffix},
		} {
			if part.value != "" {
				js.Name.Components = append(js.Name.Components, JSNameComponent{Kind: part.kind, Value: part.value})
			}
		}
	}

	for _, l := range c.Lines {
		if !handled[l.Name] && !strings.HasPrefix(l.Name, "X-AB") {
			js.VCardProps = append(js.VCardProps, []any{strings.ToLower(l.Name), params(l.Params), "unknown", l.Text()})
		}

		switch l.Name {
		case "NICKNAME":
			for nick := range strings.SplitSeq(l.Text(), ",") {
				setMap(&js.Nicknames, id("k"), JSNickname{Name: strings.TrimSpace(nick)})
			}
		case "ORG":
			parts := l.Components()
			org := JSOrganization{Name: parts[0], Units: nil}

			for _, u := range parts[1:] {
				if u != "" {
					org.Units = append(org.Units, map[string]any{"name": u})
				}
			}

			setMap(&js.Organizations, id("o"), org)
		case "TITLE", "ROLE":
			kind := "title"
			if l.Name == "ROLE" {
				kind = "role"
			}

			setMap(&js.Titles, id("t"), JSTitle{Name: l.Text(), Kind: kind})
		case "NOTE":
			setMap(&js.Notes, id("n"), JSNote{Note: l.Text()})
		case "URL":
			setMap(&js.Links, id("l"), JSLink{URI: l.Text(), Label: c.Label(l)})
		case "SOCIALPROFILE", "X-SOCIALPROFILE", "IMPP":
			svc := firstParam(l.Param("SERVICE-TYPE"))
			if svc == "" {
				svc = firstParam(l.Param("TYPE"))
			}

			setMap(&js.OnlineSvcs, id("s"), JSOnlineService{Service: svc, URI: l.Text(), User: firstParam(l.Param("X-USER"))})
		case "PRONOUNS":
			if js.SpeakToAs == nil {
				js.SpeakToAs = &JSSpeakToAs{GrammaticalGender: "", Pronouns: nil}
			}

			js.SpeakToAs.Pronouns = setIn(js.SpeakToAs.Pronouns, id("p"), JSPronoun{Pronouns: l.Text()})
		case "GRAMGENDER":
			if js.SpeakToAs == nil {
				js.SpeakToAs = &JSSpeakToAs{GrammaticalGender: "", Pronouns: nil}
			}

			js.SpeakToAs.GrammaticalGender = strings.ToLower(l.Text())
		case "MEMBER", "X-ADDRESSBOOKSERVER-MEMBER":
			js.Members = setIn(js.Members, strings.TrimPrefix(l.Text(), "urn:uuid:"), true)
		case "CATEGORIES":
			for cat := range strings.SplitSeq(l.Text(), ",") {
				js.Keywords = setIn(js.Keywords, strings.TrimSpace(cat), true)
			}
		case "PHOTO", "LOGO":
			if !photos {
				continue
			}

			if p, err := decodePhotoLine(l); err == nil {
				uri := "data:" + p.MediaType + ";base64," + strings.Map(dropSpace, l.Value)
				if strings.HasPrefix(l.Value, "data:") {
					uri = l.Value
				}

				setMap(&js.Media, id("m"), JSMedia{Kind: strings.ToLower(l.Name), URI: uri, MediaType: p.MediaType})
			} else if strings.Contains(l.Value, "://") {
				setMap(&js.Media, id("m"), JSMedia{Kind: strings.ToLower(l.Name), URI: l.Value, MediaType: ""})
			}
		}
	}

	for _, v := range c.Values("EMAIL") {
		setMap(&js.Emails, id("e"), JSEmail{Address: v.Value, Contexts: contexts(v.Types), Pref: pref(v), Label: v.Label})
	}

	for _, v := range c.Values("TEL") {
		setMap(&js.Phones, id("p"), JSPhone{Number: v.Value, Features: features(v.Types), Contexts: contexts(v.Types), Pref: pref(v), Label: v.Label})
	}

	for _, v := range c.Values("ADR") {
		setMap(&js.Addresses, id("a"), JSAddress{Full: v.Value, Contexts: contexts(v.Types), Pref: pref(v)})
	}

	for _, d := range c.Dates() {
		kind := "birth"
		if d.Kind != Birthday {
			kind = "wedding"
			if d.Kind == OtherDate {
				kind = "other"
			}
		}

		setMap(&js.Anniversaries, id("d"), JSAnniversary{
			Kind:  kind,
			Date:  PartialDate{Type: "PartialDate", Year: d.Year, Month: int(d.Month), Day: d.Day},
			Label: d.Label,
		})
	}

	return js
}

func setMap[V any](m *map[string]V, key string, v V) { *m = setIn(*m, key, v) }

func setIn[V any](m map[string]V, key string, v V) map[string]V {
	if m == nil {
		m = map[string]V{}
	}

	m[key] = v

	return m
}

func params(ps []vcf.Param) map[string]string {
	out := map[string]string{}

	for _, p := range ps {
		out[strings.ToLower(p.Name)] = strings.Join(p.Values, ",")
	}

	return out
}

func contexts(types []string) map[string]bool {
	var out map[string]bool

	for _, t := range types {
		switch t {
		case "home":
			out = setIn(out, "private", true)
		case "work":
			out = setIn(out, "work", true)
		}
	}

	return out
}

func features(types []string) map[string]bool {
	var out map[string]bool

	for _, t := range types {
		switch t {
		case "cell", "iphone":
			out = setIn(out, "mobile", true)
		case "voice", "fax", "pager", "text", "video", "textphone":
			out = setIn(out, t, true)
		case "main":
			out = setIn(out, "main-number", true)
		}
	}

	return out
}

func pref(v Value) int {
	if v.Preferred {
		return 1
	}

	return 0
}

// String returns a short description, used in logs.
func (js JSContact) String() string { return fmt.Sprintf("JSContact(%s)", js.UID) }
