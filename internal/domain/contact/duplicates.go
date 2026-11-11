package contact

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/nyaruka/phonenumbers"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// Entry is one card of an address book with its id (the file name).
type Entry struct {
	ID   string
	Card Card
}

// DuplicateGroup is a set of cards that look like the same contact, with
// what they share.
type DuplicateGroup struct {
	IDs     []string `json:"ids"`
	Names   []string `json:"names"`
	Reasons []string `json:"reasons"`
}

// Duplicates groups cards that share a phone number, an email address or
// a name. A company and its people, or two people of one company, are not
// joined by a shared number or email. A number on two cards matters beyond tidiness: whatever names an
// SMS sender or a caller from the address book picks one of the cards.
// Numbers are compared in E.164 when they can be read in regions, so
// "0912 345 6789" on one card and "+98 912 345 6789" on another match.
func Duplicates(entries []Entry, regions []string) []DuplicateGroup {
	parent := make([]int, len(entries))
	for i := range parent {
		parent[i] = i
	}

	var find func(int) int

	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}

		return parent[i]
	}

	keys, owners := duplicateKeys(entries, regions)
	// key → an entry of a pair the key joined.
	linked := map[string]int{}

	reasons := map[int][]string{}

	for _, k := range keys {
		o := owners[k]
		first := -1

		for x, i := range o {
			for _, j := range o[x+1:] {
				if !strings.HasPrefix(k, "name ") && colleagues(entries[i].Card, entries[j].Card) {
					continue
				}

				parent[find(j)] = find(i)

				if first < 0 {
					first = i
				}
			}
		}

		if first >= 0 {
			// Reasons are gathered once every key is joined, below.
			linked[k] = first
		}
	}

	for _, k := range keys {
		if i, ok := linked[k]; ok {
			r := find(i)
			reasons[r] = append(reasons[r], describeKey(k, entries[i].Card))
		}
	}

	members := map[int][]int{}

	var roots []int

	for i := range entries {
		r := find(i)
		if _, dup := reasons[r]; !dup {
			continue
		}

		if _, ok := members[r]; !ok {
			roots = append(roots, r)
		}

		members[r] = append(members[r], i)
	}

	out := make([]DuplicateGroup, 0, len(roots))

	for _, r := range roots {
		g := DuplicateGroup{IDs: nil, Names: nil, Reasons: reasons[r]}

		for _, i := range members[r] {
			g.IDs = append(g.IDs, entries[i].ID)
			g.Names = append(g.Names, entries[i].Card.DisplayName())
		}

		out = append(out, g)
	}

	slices.SortStableFunc(out, func(a, b DuplicateGroup) int {
		return strings.Compare(strings.ToLower(a.Names[0]), strings.ToLower(b.Names[0]))
	})

	return out
}

// duplicateKeys lists the keys two cards are compared by (numbers,
// emails, names), in the order first seen, with the entries that have
// each key, each once.
func duplicateKeys(entries []Entry, regions []string) ([]string, map[string][]int) {
	owners := map[string][]int{}

	var keys []string

	add := func(key string, i int) {
		if key == "" {
			return
		}

		if _, ok := owners[key]; !ok {
			keys = append(keys, key)
		}

		if !slices.Contains(owners[key], i) {
			owners[key] = append(owners[key], i)
		}
	}

	for i, e := range entries {
		// A group is not a contact: one named like a person is not that
		// person, and merging the two would lose the group.
		if IsGroup(e.Card) {
			continue
		}

		for _, l := range e.Card.All("TEL") {
			raw, _ := telNumber(l)
			add(phoneKey(raw, regions), i)
		}

		for _, l := range e.Card.All("EMAIL") {
			if v := strings.ToLower(strings.TrimSpace(l.Text())); v != "" {
				add("email "+v, i)
			}
		}

		if n := nameKey(e.Card.DisplayName()); n != "" {
			add("name "+n, i)
		}
	}

	return keys, owners
}

// colleagues reports whether two cards belong to one organization: a
// company and a person who works there (their ORG is the company), or two
// people of the same company. They share a number or an email because the
// company lists its people (the CEO's address on the company card, the
// office line on every employee), not because they are one contact. One
// person saved twice still has one name, which joins them.
func colleagues(a, b Card) bool {
	if employs(a, b) || employs(b, a) {
		return true
	}

	org := nameKey(a.Organization())

	return !IsCompany(a) && !IsCompany(b) && org != "" && org == nameKey(b.Organization())
}

func employs(company, person Card) bool {
	if !IsCompany(company) || IsCompany(person) {
		return false
	}

	org := nameKey(person.Organization())

	return org != "" && (org == nameKey(company.Organization()) || org == nameKey(company.DisplayName()))
}

// IsCompany reports whether a card is an organization: KIND:org (vCard
// 4.0, or Apple's X-ADDRESSBOOKSERVER-KIND) or Apple's X-ABShowAs:COMPANY.
func IsCompany(c Card) bool {
	return c.Kind() == "org" || strings.EqualFold(c.Value("X-ABSHOWAS"), "COMPANY")
}

// describeKey turns a grouping key back into words.
func describeKey(key string, c Card) string {
	switch {
	case strings.HasPrefix(key, "email "):
		return "same email " + strings.TrimPrefix(key, "email ")
	case strings.HasPrefix(key, "name "):
		return "same name " + quote(c.DisplayName())
	case strings.HasPrefix(key, "sender "):
		return "same sender name " + quote(strings.TrimPrefix(key, "sender "))
	default:
		return "same number " + key
	}
}

// phoneKey is the form two numbers on different cards are compared in:
// E.164 when the number can be read, "+" and its digits when it carries a
// country code the metadata does not know (an SMS panel sender), and the
// digits otherwise. A sender name saved as a number ("MCI Club") is
// compared by its letters and digits. Short codes are left out: "110" on
// two cards is not one contact.
func phoneKey(raw string, regions []string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	if strings.IndexFunc(raw, unicode.IsLetter) >= 0 && !strings.Contains(strings.ToLower(raw), "ext") {
		if k := nameKey(raw); len(k) >= 3 {
			return "sender " + k
		}

		return ""
	}

	if serviceNumber(raw) {
		return ""
	}

	if num := readTel(raw, regions); num != nil {
		return phonenumbers.Format(num, phonenumbers.E164)
	}

	digits := phonenumbers.NormalizeDigitsOnly(raw)

	if hasCountryCode(raw) {
		return "+" + strings.TrimPrefix(digits, "00")
	}

	// "983-000-86492": an Iranian sender saved without its "+".
	for _, r := range regions {
		code := strconv.Itoa(phonenumbers.GetCountryCodeForRegion(strings.ToUpper(r)))
		if code != "0" && strings.HasPrefix(digits, code) && len(digits) >= 10 {
			return "+" + digits
		}
	}

	return digits
}

// nameKey compares names: letters and digits only, in lower case, so
// "HAMRAH_AVAL" and "Hamrah Aval" are one.
func nameKey(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}

		return -1
	}, s)
}

// One of these on keep wins; other's is copied only when keep has none.
var mergeSkip = map[string]bool{
	"BEGIN": true, "END": true, "VERSION": true, "PRODID": true, "REV": true, "UID": true,
	"FN": true, "N": true, "X-ABLABEL": true, "KIND": true, "X-ADDRESSBOOKSERVER-KIND": true,
	"PHOTO": true, "X-IMAGEHASH": true, "X-IMAGETYPE": true, "X-ABSHOWAS": true,
}

// Properties a card can have many of: other's values are added when keep
// does not have them yet.
var mergeMany = map[string]bool{
	"TEL": true, "EMAIL": true, "URL": true, "ADR": true, "IMPP": true, "X-SOCIALPROFILE": true,
	"SOCIALPROFILE": true, "X-ABDATE": true, "X-ABRELATEDNAMES": true, "RELATED": true,
	"CATEGORIES": true, "NICKNAME": true,
}

// Merge returns keep with what only other has: its phone numbers, email
// addresses, links, addresses and the like that keep lacks, any property
// keep does not have at all, its note when it differs, and its photo when
// keep has none or a smaller one. The lines keep already has stay
// byte-exact; a copied line that belongs to a group (item3.TEL with its
// X-ABLabel) comes with its label, renamed to a group keep does not use.
// Both cards must carry their photos (not Slim) for the photo to be
// compared.
func Merge(keep, other Card, regions []string) Card {
	m := merger{out: keep.Clone(), other: other, regions: regions, used: map[string]bool{}, renamed: map[string]string{}, next: 1}

	for _, l := range m.out.Lines {
		if l.Group != "" {
			m.used[strings.ToLower(l.Group)] = true
		}
	}

	for _, l := range other.Lines {
		switch {
		case mergeSkip[l.Name]:
		case mergeMany[l.Name]:
			if !m.has(l.Name, l.Text()) {
				m.copyLine(l)
			}
		case l.Name == "NOTE":
			m.note(l)
		default:
			if _, ok := m.out.Get(l.Name); !ok {
				m.copyLine(l)
			}
		}
	}

	if better(other, keep) {
		m.out = WithPhotoOf(m.out, other)
	}

	return m.out
}

// merger is the state of one Merge.
type merger struct {
	out, other Card
	regions    []string
	used       map[string]bool   // groups out has
	renamed    map[string]string // other's group → its name in out
	next       int
}

// group returns the name other's group g gets in out.
func (m *merger) group(g string) string {
	if g == "" {
		return ""
	}

	if n, ok := m.renamed[g]; ok {
		return n
	}

	for m.used["item"+strconv.Itoa(m.next)] {
		m.next++
	}

	n := "item" + strconv.Itoa(m.next)
	m.used[n], m.renamed[g] = true, n

	return n
}

func (m *merger) has(name, value string) bool {
	return slices.ContainsFunc(m.out.Lines, func(l vcf.Line) bool { return l.Name == name && sameValue(name, l, value, m.regions) })
}

// copyLine adds a line of other to out, with its group's label.
func (m *merger) copyLine(l vcf.Line) {
	src := l.Group
	l = Card{Lines: []vcf.Line{l}}.Clone().Lines[0]
	l.Group = m.group(src)
	m.out.insertBeforeEnd(l)

	if src == "" {
		return
	}

	for _, g := range m.other.Lines {
		if g.Group == src && g.Name == "X-ABLABEL" && !hasGroupLine(m.out, l.Group, "X-ABLABEL") {
			g = Card{Lines: []vcf.Line{g}}.Clone().Lines[0]
			g.Group = l.Group
			m.out.insertBeforeEnd(g)
		}
	}
}

// note adds other's note to out's, unless out already says it.
func (m *merger) note(l vcf.Line) {
	cur, ok := m.out.Get("NOTE")

	switch {
	case !ok:
		m.copyLine(l)
	case strings.TrimSpace(cur.Text()) != strings.TrimSpace(l.Text()) && !strings.Contains(cur.Text(), l.Text()):
		for i := range m.out.Lines {
			if m.out.Lines[i].Name == "NOTE" {
				m.out.Lines[i].Value = vcf.Escape(cur.Text() + "\n\n" + l.Text())

				break
			}
		}
	}
}

// sameValue compares a value of keep with one of other.
func sameValue(name string, l vcf.Line, value string, regions []string) bool {
	switch name {
	case "TEL":
		a, _ := telNumber(l)
		b, _ := stripScheme(value)

		ka, kb := phoneKey(a, regions), phoneKey(b, regions)
		if ka != "" && kb != "" {
			return ka == kb
		}

		return strings.TrimSpace(a) == strings.TrimSpace(b)
	case "EMAIL":
		return strings.EqualFold(strings.TrimSpace(l.Text()), strings.TrimSpace(value))
	default:
		return strings.TrimSpace(l.Text()) == strings.TrimSpace(value)
	}
}

func hasGroupLine(c Card, group, name string) bool {
	return slices.ContainsFunc(c.Lines, func(l vcf.Line) bool { return l.Group == group && l.Name == name })
}

// better reports whether a has a photo and b has none or a smaller one.
func better(a, b Card) bool {
	pa, okA := a.PhotoInfo()
	if !okA {
		return false
	}

	pb, okB := b.PhotoInfo()
	if !okB {
		return true
	}

	return max(pa.Width, pa.Height) > max(pb.Width, pb.Height)
}
