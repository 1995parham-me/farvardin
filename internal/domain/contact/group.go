package contact

import (
	"slices"
	"strings"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// Contact groups.
//
// A group is a card of its own: Apple's X-ADDRESSBOOKSERVER-KIND:group with
// one X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:<uid> line per member in vCard
// 3.0, or KIND:group with MEMBER in vCard 4.0. iOS writes groups this way,
// and so does DAVx5 when its account keeps "groups as separate vCards",
// which turns each into an Android contact group. CATEGORIES on a card is
// the other way to group (DAVx5's "groups as per-contact categories"); it
// is read as well, so a book written either way lists its groups.

// GroupKind is the KIND of a group card.
const GroupKind = "group"

const memberPrefix = "urn:uuid:"

// IsGroup reports whether a card is a group card.
func IsGroup(c Card) bool { return c.Kind() == GroupKind }

// NewGroup returns an empty vCard 3.0 group card named name.
func NewGroup(uid, name string) Card {
	g := New(uid, name)
	g.SetGroupName(name)
	g.SetText("X-ADDRESSBOOKSERVER-KIND", GroupKind)

	return g
}

// SetGroupName sets a group's FN, and its N to the same name in the
// family slot, the way iOS writes a group.
func (c *Card) SetGroupName(name string) {
	c.SetText("FN", name)
	c.Replace("N", []vcf.Line{line("N", vcf.Escape(name)+";;;;")})
}

// memberProperty is the member property this card's version uses.
func (c Card) memberProperty() string {
	if c.Version() == V4 {
		return "MEMBER"
	}

	return "X-ADDRESSBOOKSERVER-MEMBER"
}

// Members returns the UIDs of a group's members, in order, without the
// urn:uuid: prefix and without repeats.
func (c Card) Members() []string {
	var out []string

	for _, name := range []string{"X-ADDRESSBOOKSERVER-MEMBER", "MEMBER"} {
		for _, l := range c.All(name) {
			uid := MemberUID(l.Text())
			if uid != "" && !slices.Contains(out, uid) {
				out = append(out, uid)
			}
		}
	}

	return out
}

// MemberUID turns a member reference (urn:uuid:<uid>, or a bare UID) into
// the UID it names.
func MemberUID(ref string) string {
	ref = strings.TrimSpace(ref)
	if len(ref) >= len(memberPrefix) && strings.EqualFold(ref[:len(memberPrefix)], memberPrefix) {
		ref = ref[len(memberPrefix):]
	}

	return ref
}

// SetMembers replaces a group's members with uids, in that order, written
// in the member property of the card's version.
func (c *Card) SetMembers(uids []string) {
	prop := c.memberProperty()

	var lines []vcf.Line

	seen := map[string]bool{}

	for _, uid := range uids {
		uid = MemberUID(uid)
		if uid == "" || seen[uid] {
			continue
		}

		seen[uid] = true
		lines = append(lines, line(prop, memberPrefix+uid))
	}

	c.Replace("X-ADDRESSBOOKSERVER-MEMBER", nil)
	c.Replace("MEMBER", nil)
	c.Replace(prop, lines)
}

// Categories returns the card's CATEGORIES, every line and every
// comma-separated value, trimmed and without repeats.
func (c Card) Categories() []string {
	var out []string

	for _, l := range c.All("CATEGORIES") {
		for _, raw := range vcf.SplitUnescaped(l.Value, ',') {
			v := strings.TrimSpace(vcf.Unescape(raw))
			if v != "" && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}

	return out
}
