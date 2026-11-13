package contact_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
)

const apple = "BEGIN:VCARD\r\n" +
	"VERSION:3.0\r\n" +
	"PRODID:-//Apple Inc.//iOS 18.0//EN\r\n" +
	"N:Doe;Jane;;Dr.;\r\n" +
	"FN:Dr. Jane Doe\r\n" +
	"NICKNAME:ژاله\r\n" +
	"EMAIL;type=INTERNET;type=HOME;type=pref:jane@example.com\r\n" +
	"TEL;type=CELL;type=VOICE;type=pref:+15555550100\r\n" +
	"item1.X-ABDATE;type=pref:2024-06-16\r\n" +
	"item1.X-ABLabel:_$!<Anniversary>!$_\r\n" +
	"BDAY;X-APPLE-OMIT-YEAR=1604:1604-03-16\r\n" +
	"X-SOCIALPROFILE;type=github;x-user=janedoe:https://github.com/janedoe\r\n" +
	"UID:me\r\n" +
	"END:VCARD\r\n"

func mustParse(t *testing.T, s string) contact.Card {
	t.Helper()

	c, err := contact.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func TestParseAndFields(t *testing.T) {
	t.Parallel()

	c := mustParse(t, apple)

	if c.DisplayName() != "Dr. Jane Doe" || c.Name().Given != "Jane" || c.Name().Prefix != "Dr." {
		t.Fatalf("name: %q %+v", c.DisplayName(), c.Name())
	}

	emails := c.Values("EMAIL")
	if len(emails) != 1 || !emails[0].Preferred || emails[0].Types[0] != "internet" {
		t.Fatalf("emails: %+v", emails)
	}

	dates := c.Dates()
	if len(dates) != 2 {
		t.Fatalf("dates: %+v", dates)
	}

	if dates[0].Kind != contact.Anniversary || dates[1].Kind != contact.Birthday || dates[1].Year != 0 || dates[1].Day != 16 {
		t.Fatalf("dates: %+v", dates)
	}

	from := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if next := dates[1].Next(from); next.Format("2006-01-02") != "2027-03-16" {
		t.Fatalf("next birthday %s", next)
	}

	if issues := contact.Validate(c); contact.HasErrors(issues) {
		t.Fatalf("valid card reported: %v", issues)
	}
}

func TestEncodeFoldsEditedLines(t *testing.T) {
	t.Parallel()

	c := mustParse(t, apple)
	c.Set("NOTE", strings.Repeat("بهار ", 40))
	out := c.Encode()

	for line := range strings.SplitSeq(string(out), "\r\n") {
		if len(line) > 75 {
			t.Fatalf("edited line longer than 75 octets: %d", len(line))
		}
	}

	again := mustParse(t, string(out))
	if again.Value("NOTE") != c.Value("NOTE") {
		t.Fatal("fold/unfold changed the value")
	}
}

func TestConvertBothWays(t *testing.T) {
	t.Parallel()

	v4, err := contact.Convert(mustParse(t, apple), contact.V4)
	if err != nil {
		t.Fatal(err)
	}

	out := string(v4.Encode())
	for _, want := range []string{
		"VERSION:4.0", "EMAIL;PREF=1;TYPE=home:jane@example.com", "BDAY:--0316",
		"item1.ANNIVERSARY", "SOCIALPROFILE;x-user=janedoe;SERVICE-TYPE=github:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("4.0 output lacks %q:\n%s", want, out)
		}
	}

	v3, err := contact.Convert(v4, contact.V3)
	if err != nil {
		t.Fatal(err)
	}

	back := string(v3.Encode())
	for _, want := range []string{"VERSION:3.0", "BDAY;X-APPLE-OMIT-YEAR=1604:1604-03-16", "X-SOCIALPROFILE", "TYPE=home,pref"} {
		if !strings.Contains(back, want) {
			t.Fatalf("3.0 output lacks %q:\n%s", want, back)
		}
	}
}

func TestJSContact(t *testing.T) {
	t.Parallel()

	js := contact.ToJSContact(mustParse(t, apple), false)

	if js.UID != "me" || js.Name.Full != "Dr. Jane Doe" || len(js.Emails) != 1 || len(js.Anniversaries) != 2 {
		t.Fatalf("jscontact: %+v", js)
	}

	if js.Phones["p1"].Features["mobile"] != true || js.OnlineSvcs["s1"].Service != "github" {
		t.Fatalf("phones/online: %+v %+v", js.Phones, js.OnlineSvcs)
	}
}

func jpegOf(t *testing.T, w, h int, shade uint8, quality int) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: shade, G: uint8(x * 255 / w), B: uint8(y * 255 / h), A: 255})
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func withPhoto(data string) string {
	return "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:p\r\nFN:P\r\nPHOTO;ENCODING=b;TYPE=JPEG:" + data + "\r\nEND:VCARD\r\n"
}

func TestPhotoPolicy(t *testing.T) {
	t.Parallel()

	policy := contact.PhotoPolicy{MaxSize: 720, Guard: true, Stabilize: true, ThumbnailSize: 128, Quality: 85}

	big := mustParse(t, withPhoto(jpegOf(t, 720, 720, 100, 90)))

	// A thumbnail of the stored photo is refused; the big one stays.
	thumb := mustParse(t, withPhoto(jpegOf(t, 96, 96, 100, 90)))

	out, action := contact.ApplyPhotoPolicy(thumb, &big, policy)
	if action != contact.PhotoGuarded {
		t.Fatalf("thumbnail: action %q", action)
	}

	if p, _ := out.Photo(); p.Width != 720 {
		t.Fatalf("guarded photo is %dpx", p.Width)
	}

	// The same picture encoded again keeps the stored bytes.
	reencoded := mustParse(t, withPhoto(jpegOf(t, 720, 720, 100, 70)))

	out, action = contact.ApplyPhotoPolicy(reencoded, &big, policy)
	if action != contact.PhotoStabilized || out.Value("PHOTO") != big.Value("PHOTO") {
		t.Fatalf("re-encode: action %q", action)
	}

	// A different picture of the same size is a real change.
	other := mustParse(t, withPhoto(jpegOf(t, 720, 720, 250, 90)))
	if _, action = contact.ApplyPhotoPolicy(other, &big, policy); action != contact.PhotoUnchanged {
		t.Fatalf("new picture: action %q", action)
	}

	// A photo above MaxSize is scaled down.
	huge := mustParse(t, withPhoto(jpegOf(t, 2000, 1000, 50, 90)))

	out, action = contact.ApplyPhotoPolicy(huge, nil, policy)
	if p, _ := out.Photo(); action != contact.PhotoScaled || p.Width != 720 || p.Height != 360 {
		t.Fatalf("scale: action %q, %dx%d", action, p.Width, p.Height)
	}
}

func withPhoto4(data string) string {
	return "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:p\r\nFN:P\r\nPHOTO:data:image/jpeg;base64," + data + "\r\nEND:VCARD\r\n"
}

// assertPhotoFits fails unless the card's PHOTO is written the way its
// VERSION wants it: a data: URI in 4.0, ENCODING=b in 3.0.
func assertPhotoFits(t *testing.T, what string, c contact.Card) {
	t.Helper()

	l, ok := c.Get("PHOTO")
	if !ok {
		t.Fatalf("%s: no PHOTO", what)
	}

	isURI := strings.HasPrefix(l.Value, "data:image/jpeg;base64,")
	hasEnc := len(l.Param("ENCODING")) > 0

	switch c.Version() {
	case contact.V4:
		if !isURI || hasEnc {
			t.Fatalf("%s: 4.0 card has PHOTO %v:%.40s", what, l.Params, l.Value)
		}
	case contact.V3:
		if isURI || !hasEnc {
			t.Fatalf("%s: 3.0 card has PHOTO %v:%.40s", what, l.Params, l.Value)
		}
	}

	if _, err := c.Photo(); err != nil {
		t.Fatalf("%s: photo does not decode: %v", what, err)
	}
}

// A phone that moves a card from 3.0 to 4.0 must not get the stored 3.0
// photo line back inside its 4.0 card: DAVx5 reads such a line as a URL
// and drops the photo.
func TestPhotoPolicyAcrossVersions(t *testing.T) {
	t.Parallel()

	policy := contact.PhotoPolicy{MaxSize: 720, Guard: true, Stabilize: true, ThumbnailSize: 128, Quality: 85}

	big3 := mustParse(t, withPhoto(jpegOf(t, 720, 720, 100, 90)))
	big4 := mustParse(t, withPhoto4(jpegOf(t, 720, 720, 100, 90)))

	// Stabilized: the same picture, re-encoded, uploaded as 4.0.
	out, action := contact.ApplyPhotoPolicy(mustParse(t, withPhoto4(jpegOf(t, 720, 720, 100, 70))), &big3, policy)
	if action != contact.PhotoStabilized {
		t.Fatalf("stabilize 3→4: action %q", action)
	}

	assertPhotoFits(t, "stabilize 3→4", out)

	// Guarded: a thumbnail uploaded as 4.0 over a stored 3.0 card.
	out, action = contact.ApplyPhotoPolicy(mustParse(t, withPhoto4(jpegOf(t, 96, 96, 100, 90))), &big3, policy)
	if action != contact.PhotoGuarded {
		t.Fatalf("guard 3→4: action %q", action)
	}

	assertPhotoFits(t, "guard 3→4", out)

	// And the other way: a 3.0 upload over a stored 4.0 card.
	out, _ = contact.ApplyPhotoPolicy(mustParse(t, withPhoto(jpegOf(t, 96, 96, 100, 90))), &big4, policy)
	assertPhotoFits(t, "guard 4→3", out)

	// Scaled: a big 4.0 photo stays a data: URI.
	out, action = contact.ApplyPhotoPolicy(mustParse(t, withPhoto4(jpegOf(t, 2000, 1000, 50, 90))), nil, policy)
	if action != contact.PhotoScaled {
		t.Fatalf("scale 4.0: action %q", action)
	}

	assertPhotoFits(t, "scale 4.0", out)

	// Fitted: a card already broken (3.0 photo line in a 4.0 card) is
	// repaired when written, even with nothing else to do.
	broken := strings.Replace(withPhoto(jpegOf(t, 300, 300, 100, 90)), "VERSION:3.0", "VERSION:4.0", 1)

	out, action = contact.ApplyPhotoPolicy(mustParse(t, broken), nil, policy)
	if action != contact.PhotoFitted {
		t.Fatalf("broken card: action %q", action)
	}

	assertPhotoFits(t, "fitted", out)

	// A card already right is left byte for byte.
	ok := withPhoto4(jpegOf(t, 300, 300, 100, 90))
	if got := contact.FitBinary(mustParse(t, ok)).Encode(); string(got) != ok {
		t.Fatal("FitBinary changed a 4.0 card that was already right")
	}
}

// Restoring a photo from a 3.0 revision into a 4.0 card writes it as 4.0.
func TestWithPhotoOfAcrossVersions(t *testing.T) {
	t.Parallel()

	old3 := mustParse(t, withPhoto(jpegOf(t, 300, 300, 100, 90)))
	cur4 := mustParse(t, withPhoto4(jpegOf(t, 300, 300, 250, 90)))

	assertPhotoFits(t, "3.0 photo into 4.0", contact.WithPhotoOf(cur4, old3))
	assertPhotoFits(t, "4.0 photo into 3.0", contact.WithPhotoOf(old3, cur4))
}

func TestValidateErrors(t *testing.T) {
	t.Parallel()

	c := mustParse(t, "BEGIN:VCARD\r\nVERSION:2.1\r\nEND:VCARD\r\n")

	issues := contact.Validate(c)
	if !contact.HasErrors(issues) || len(issues) < 3 {
		t.Fatalf("issues: %v", issues)
	}

	if _, err := contact.Parse([]byte(apple + apple)); err == nil {
		t.Fatal("two cards in one resource must fail")
	}
}

// An iOS card keeps unfolded long lines; a card that is decoded and
// encoded again without edits must come back byte for byte.
func TestEncodeKeepsUntouchedBytes(t *testing.T) {
	t.Parallel()

	in := strings.Replace(apple, "UID:me\r\n", "item2.X-ADDRESSING-GRAMMAR:"+strings.Repeat("Q", 300)+"\r\nUID:me\r\n", 1)
	in = strings.Replace(in, "NICKNAME:ژاله\r\n", "NOTE:folded the\r\n  Apple way\n", 1)

	c := mustParse(t, in)
	if string(c.Encode()) != in {
		t.Fatalf("round trip changed bytes:\n%q\n%q", in, c.Encode())
	}

	c.Set("FN", "Changed")

	out := string(c.Encode())
	if !strings.Contains(out, strings.Repeat("Q", 300)) || !strings.Contains(out, "FN:Changed\r\n") {
		t.Fatalf("edit touched other lines:\n%s", out)
	}
}
