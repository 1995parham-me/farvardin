package contact

import (
	"fmt"
	"slices"
	"strings"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// Supported vCard versions.
const (
	V3 = "3.0"
	V4 = "4.0"
)

// binaryProps carry inline data: ENCODING=b in 3.0, data: URIs in 4.0.
var binaryProps = []string{"PHOTO", "LOGO", "SOUND", "KEY"}

// renames3to4 maps 3.0 / Apple extension names to their RFC 6350 and
// RFC 9554 equivalents. The reverse map is used for 4.0 to 3.0.
var renames3to4 = map[string]string{
	"X-ADDRESSBOOKSERVER-KIND":   "KIND",
	"X-ADDRESSBOOKSERVER-MEMBER": "MEMBER",
	"X-ANNIVERSARY":              "ANNIVERSARY",
	"X-SOCIALPROFILE":            "SOCIALPROFILE",
	"X-GENDER":                   "GENDER",
}

// Convert returns a copy of the card in the requested version (V3 or V4).
// A card already in that version is returned unchanged.
func Convert(c Card, version string) (Card, error) {
	switch version {
	case V3, V4:
	default:
		return Card{}, fmt.Errorf("unsupported vCard version %q", version)
	}

	if c.Version() == version {
		return c, nil
	}

	out := c.Clone()
	lines := make([]vcf.Line, 0, len(out.Lines))

	for _, l := range out.Lines {
		var keep bool

		if version == V4 {
			l, keep = to4(out, l)
		} else {
			l, keep = to3(l)
		}

		if keep {
			lines = append(lines, l)
		}
	}

	out.Lines = lines
	out.Set("VERSION", version)

	return out, nil
}

func to4(c Card, l vcf.Line) (vcf.Line, bool) {
	if n, ok := renames3to4[l.Name]; ok {
		l.Name = n
	}

	l.SetParam("CHARSET")

	// TYPE=pref becomes PREF=1, TYPE=internet disappears, the rest is
	// lower case.
	if types := l.Param("TYPE"); len(types) > 0 {
		var kept []string

		for _, t := range types {
			switch t = strings.ToLower(t); t {
			case "pref":
				l.SetParam("PREF", "1")
			case "internet", "x400":
			default:
				kept = append(kept, t)
			}
		}

		l.SetParam("TYPE", kept...)
	}

	switch {
	case slices.Contains(binaryProps, l.Name):
		enc := strings.ToLower(firstParam(l.Param("ENCODING")))
		if enc == "b" || enc == "base64" {
			media := mediaType(l.Name, firstParam(l.Param("TYPE")))
			l.Value = "data:" + media + ";base64," + l.Value
			l.SetParam("ENCODING")
			l.SetParam("TYPE")
		}
	case l.Name == "SOCIALPROFILE":
		if t := l.Param("TYPE"); len(t) > 0 {
			l.SetParam("SERVICE-TYPE", t...)
			l.SetParam("TYPE")
		}
	case l.Name == "BDAY" || l.Name == "ANNIVERSARY":
		if len(l.Param("X-APPLE-OMIT-YEAR")) > 0 || strings.HasPrefix(l.Value, "1604") {
			if d, ok := ParseDate(l.Value); ok {
				l.Value = fmt.Sprintf("--%02d%02d", int(d.Month), d.Day)
			}

			l.SetParam("X-APPLE-OMIT-YEAR")
		}
	case l.Name == "X-ABDATE" && strings.EqualFold(c.Label(l), "Anniversary"):
		l.Name = "ANNIVERSARY"
	case l.Name == "X-ABRELATEDNAMES":
		l.Name = "RELATED"
		l.SetParam("VALUE", "text")

		if label := strings.ToLower(c.Label(l)); slices.Contains(relatedTypes, label) {
			l.SetParam("TYPE", label)
		}
	}

	return l, true
}

// relatedTypes are the RELATED types of RFC 6350 section 6.6.6.
var relatedTypes = []string{
	"contact", "acquaintance", "friend", "met", "co-worker", "colleague",
	"co-resident", "neighbor", "child", "parent", "sibling", "spouse", "kin",
	"muse", "crush", "date", "sweetheart", "me", "agent", "emergency",
}

func to3(l vcf.Line) (vcf.Line, bool) {
	for v3, v4 := range renames3to4 {
		if l.Name == v4 {
			l.Name = v3
		}
	}

	if len(l.Param("PREF")) > 0 {
		l.SetParam("PREF")
		l.SetParam("TYPE", append(l.Param("TYPE"), "pref")...)
	}

	switch {
	case slices.Contains(binaryProps, l.Name):
		if media, data, ok := splitDataURI(l.Value); ok {
			l.Value = data
			l.SetParam("ENCODING", "b")
			l.SetParam("TYPE", strings.ToUpper(strings.TrimPrefix(media[strings.IndexByte(media, '/')+1:], "x-")))
			l.SetParam("MEDIATYPE")
		}
	case l.Name == "X-SOCIALPROFILE":
		if t := l.Param("SERVICE-TYPE"); len(t) > 0 {
			l.SetParam("TYPE", t...)
			l.SetParam("SERVICE-TYPE")
		}
	case l.Name == "BDAY" || l.Name == "X-ANNIVERSARY":
		if rest, ok := strings.CutPrefix(l.Value, "--"); ok && len(strings.ReplaceAll(rest, "-", "")) == 4 {
			md := strings.ReplaceAll(rest, "-", "")
			l.Value = "1604-" + md[:2] + "-" + md[2:]
			l.SetParam("X-APPLE-OMIT-YEAR", "1604")
		}
	}

	return l, true
}

func mediaType(prop, typ string) string {
	typ = strings.ToLower(typ)
	if typ == "" {
		typ = "jpeg"
	}

	if strings.Contains(typ, "/") {
		return typ
	}

	switch prop {
	case "SOUND":
		return "audio/" + typ
	case "KEY":
		return "application/" + typ
	default:
		return "image/" + typ
	}
}

// splitDataURI splits "data:image/jpeg;base64,AAAA" into its media type
// and base64 payload.
func splitDataURI(v string) (string, string, bool) {
	rest, ok := strings.CutPrefix(v, "data:")
	if !ok {
		return "", "", false
	}

	meta, data, ok := strings.Cut(rest, ",")
	if !ok {
		return "", "", false
	}

	media, enc, _ := strings.Cut(meta, ";")
	if enc != "base64" {
		return "", "", false
	}

	if media == "" {
		media = "image/jpeg"
	}

	return media, data, true
}
