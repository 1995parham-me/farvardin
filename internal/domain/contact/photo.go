package contact

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/jpeg"
	"slices"
	"strconv"
	"strings"

	// Register the decoders contact clients use.
	_ "image/gif"
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/1995parham-me/farvardin/internal/domain/vcf"
)

// Errors about photos.
var (
	ErrNoPhoto = errors.New("card has no inline photo")
	// ErrSlim is returned for the photo bytes of a slim card.
	ErrSlim = errors.New("photo bytes are not kept on a slim card; parse the stored object")
	// ErrTooLarge refuses images whose pixel count could exhaust memory.
	ErrTooLarge = errors.New("image is too large to decode")
)

// MaxPixels bounds the images farvardin decodes (a JPEG of a few KB can
// declare 30000x30000 and allocate gigabytes). 16 MP covers any phone
// contact photo.
var MaxPixels int64 = 16_000_000

// decodeSlots limits concurrent full image decodes; each can take tens of
// MB and a slow CPU, so two at a time is plenty on a small board.
var decodeSlots = make(chan struct{}, 2)

func decodeImage(data []byte) (image.Image, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err //nolint:wrapcheck // callers only check for nil
	}

	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, ErrTooLarge
	}

	decodeSlots <- struct{}{}
	defer func() { <-decodeSlots }()

	img, _, err := image.Decode(bytes.NewReader(data))

	return img, err //nolint:wrapcheck // callers only check for nil
}

// PhotoInfo is what a slim card remembers about its photo.
type PhotoInfo struct {
	Width     int
	Height    int
	Bytes     int
	MediaType string
	// Key identifies the photo bytes, so equal photos compare equal.
	Key string
}

// Slim returns a copy without the values of binary properties, keeping
// the photo's size and identity. Cached cards are slim: a 1000-card
// address book costs ~2 MB instead of ~65 MB.
func (c Card) Slim() Card {
	if c.slim != nil {
		return c
	}

	info := PhotoInfo{Width: 0, Height: 0, Bytes: 0, MediaType: "", Key: c.PhotoKey()}
	if p, err := c.Photo(); err == nil {
		info.Width, info.Height, info.Bytes, info.MediaType = p.Width, p.Height, len(p.Data), p.MediaType
	}

	out := Card{Lines: make([]vcf.Line, len(c.Lines)), slim: &info}

	for i, l := range c.Lines {
		l.Params = slices.Clone(l.Params)
		if slices.Contains(binaryProps, l.Name) {
			l.DropValue()
		}

		out.Lines[i] = l
	}

	return out
}

// IsSlim reports whether the card's binary values were dropped.
func (c Card) IsSlim() bool { return c.slim != nil }

// PhotoInfo returns the photo's size and identity, from a full or a slim
// card.
func (c Card) PhotoInfo() (PhotoInfo, bool) {
	if c.slim != nil {
		return *c.slim, c.slim.Key != ""
	}

	p, err := c.Photo()
	if err != nil {
		return PhotoInfo{}, false
	}

	return PhotoInfo{Width: p.Width, Height: p.Height, Bytes: len(p.Data), MediaType: p.MediaType, Key: c.PhotoKey()}, true
}

// Photo is an inline photo decoded from a card.
type Photo struct {
	Data      []byte
	MediaType string
	Width     int
	Height    int
}

// PhotoPolicy says what to do with photos when a client writes a card.
type PhotoPolicy struct {
	// MaxSize scales photos whose longer side is larger than this. 0 = off.
	MaxSize int
	// Guard keeps the stored photo when a client sends a thumbnail of it
	// (longer side <= ThumbnailSize while the stored one is at least twice
	// as large).
	Guard bool
	// Stabilize keeps the stored photo bytes when a client sends the same
	// picture encoded again (same size, near-identical pixels). This stops
	// two clients from re-encoding one photo back and forth forever.
	Stabilize bool
	// ThumbnailSize is the longest side that counts as a thumbnail.
	ThumbnailSize int
	// Quality is the JPEG quality used when a photo is scaled.
	Quality int
}

// PhotoAction is what ApplyPhotoPolicy did.
type PhotoAction string

// Photo actions.
const (
	PhotoUnchanged  PhotoAction = ""
	PhotoScaled     PhotoAction = "scaled"
	PhotoGuarded    PhotoAction = "guarded"
	PhotoStabilized PhotoAction = "stabilized"
)

// Photo decodes the first inline PHOTO of the card.
func (c Card) Photo() (Photo, error) {
	if c.slim != nil {
		return Photo{}, ErrSlim
	}

	l, ok := c.Get("PHOTO")
	if !ok {
		return Photo{}, ErrNoPhoto
	}

	return decodePhotoLine(l)
}

func decodePhotoLine(l vcf.Line) (Photo, error) {
	var (
		media   string
		payload string
	)

	if m, d, ok := splitDataURI(l.Value); ok {
		media, payload = m, d
	} else {
		enc := strings.ToLower(firstParam(l.Param("ENCODING")))
		if enc != "b" && enc != "base64" {
			return Photo{}, ErrNoPhoto
		}

		media, payload = mediaType("PHOTO", firstParam(l.Param("TYPE"))), l.Value
	}

	data, err := base64.StdEncoding.DecodeString(strings.Map(dropSpace, payload))
	if err != nil {
		return Photo{}, errors.Join(ErrNoPhoto, err)
	}

	p := Photo{Data: data, MediaType: media, Width: 0, Height: 0}

	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		p.Width, p.Height = cfg.Width, cfg.Height
	}

	return p, nil
}

func dropSpace(r rune) rune {
	switch r {
	case ' ', '\t', '\r', '\n':
		return -1
	default:
		return r
	}
}

// ApplyPhotoPolicy changes the PHOTO of incoming (a card a client is
// writing) according to policy. stored is the version already on the
// server, or nil for a new card. It returns the card to store and what
// was done.
func ApplyPhotoPolicy(incoming Card, stored *Card, policy PhotoPolicy) (Card, PhotoAction) {
	idx := slicesIndex(incoming.Lines, "PHOTO")
	if idx < 0 {
		return incoming, PhotoUnchanged
	}

	in, err := decodePhotoLine(incoming.Lines[idx])
	if err != nil || in.Width == 0 || int64(in.Width)*int64(in.Height) > MaxPixels {
		return incoming, PhotoUnchanged
	}

	if stored != nil {
		if sidx := slicesIndex(stored.Lines, "PHOTO"); sidx >= 0 {
			old, err := decodePhotoLine(stored.Lines[sidx])
			if err == nil && old.Width > 0 {
				if policy.Guard && isThumbnailOf(in, old, policy.ThumbnailSize) {
					return replacePhoto(incoming, idx, stored.Lines[sidx]), PhotoGuarded
				}

				if policy.Stabilize && samePicture(in, old) {
					if incoming.Lines[idx].Value == stored.Lines[sidx].Value {
						return incoming, PhotoUnchanged
					}

					return replacePhoto(incoming, idx, stored.Lines[sidx]), PhotoStabilized
				}
			}
		}
	}

	if policy.MaxSize > 0 && max(in.Width, in.Height) > policy.MaxSize {
		scaled, err := scale(in, policy.MaxSize, policy.Quality)
		if err == nil {
			out := incoming.Clone()
			l := &out.Lines[idx]

			encoded := base64.StdEncoding.EncodeToString(scaled)
			if strings.HasPrefix(l.Value, "data:") {
				l.Value = "data:image/jpeg;base64," + encoded
			} else {
				l.Value = encoded
				l.SetParam("ENCODING", "b")
				l.SetParam("TYPE", "JPEG")
			}
			// Apple's crop rectangle refers to the old pixel size.
			l.SetParam("X-ABCROP-RECTANGLE")

			return out, PhotoScaled
		}
	}

	return incoming, PhotoUnchanged
}

func replacePhoto(c Card, idx int, photo vcf.Line) Card {
	out := c.Clone()
	photo.Group = out.Lines[idx].Group
	out.Lines[idx] = photo

	return out
}

func slicesIndex(lines []vcf.Line, name string) int {
	for i, l := range lines {
		if l.Name == name {
			return i
		}
	}

	return -1
}

// isThumbnailOf is true when in is small and old is at least twice as
// large with the same aspect ratio (within 5 %).
func isThumbnailOf(in, old Photo, thumb int) bool {
	if thumb <= 0 || max(in.Width, in.Height) > thumb {
		return false
	}

	if max(old.Width, old.Height) < 2*max(in.Width, in.Height) {
		return false
	}

	a := float64(in.Width) / float64(in.Height)
	b := float64(old.Width) / float64(old.Height)

	return a/b > 0.95 && a/b < 1.05
}

// samePicture compares two photos of equal size on a 16x16 grey
// thumbnail. A mean difference under 3/255 is a re-encode, not a new
// picture.
func samePicture(a, b Photo) bool {
	if a.Width != b.Width || a.Height != b.Height {
		return false
	}

	ga, err1 := fingerprint(a.Data)
	gb, err2 := fingerprint(b.Data)

	if err1 != nil || err2 != nil {
		return false
	}

	var diff int

	for i := range ga {
		d := int(ga[i]) - int(gb[i])
		if d < 0 {
			d = -d
		}

		diff += d
	}

	return diff < 3*len(ga)
}

const fingerprintSize = 16

func fingerprint(data []byte) ([]uint8, error) {
	img, err := decodeImage(data)
	if err != nil {
		return nil, err
	}

	small := image.NewGray(image.Rect(0, 0, fingerprintSize, fingerprintSize))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), img, img.Bounds(), draw.Src, nil)

	return small.Pix, nil
}

func scale(p Photo, maxSize, quality int) ([]byte, error) {
	img, err := decodeImage(p.Data)
	if err != nil {
		return nil, err
	}

	w, h := p.Width, p.Height
	if w >= h {
		w, h = maxSize, h*maxSize/w
	} else {
		w, h = w*maxSize/h, maxSize
	}

	dst := image.NewRGBA(image.Rect(0, 0, max(w, 1), max(h, 1)))

	// CatmullRom for stored photos; the cheaper bilinear filter for small
	// thumbnails, where the difference does not show and a slow CPU does.
	var scaler draw.Scaler = draw.CatmullRom
	if maxSize <= 256 {
		scaler = draw.BiLinear
	}

	scaler.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)

	if quality <= 0 {
		quality = 85
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err //nolint:wrapcheck // caller only checks for nil
	}

	return buf.Bytes(), nil
}

// Thumbnail returns the photo scaled so its longer side is at most size,
// as JPEG. A photo that is already small enough is returned as it is.
func (p Photo) Thumbnail(size int) ([]byte, string, error) {
	if size <= 0 || max(p.Width, p.Height) <= size {
		return p.Data, p.MediaType, nil
	}

	data, err := scale(p, size, 80)
	if err != nil {
		return nil, "", err
	}

	return data, "image/jpeg", nil
}

// photoLines are the properties that describe a card's photo.
var photoLines = map[string]bool{"PHOTO": true, "X-IMAGEHASH": true, "X-IMAGETYPE": true}

// WithPhotoOf returns cur with its photo replaced by the photo of from
// (PHOTO plus Apple's X-IMAGEHASH and X-IMAGETYPE), in the place cur's
// photo was, or before END:VCARD. Every other line keeps its bytes.
func WithPhotoOf(cur, from Card) Card {
	var photo []vcf.Line

	for _, l := range from.Lines {
		if photoLines[l.Name] {
			photo = append(photo, l)
		}
	}

	out := Card{Lines: make([]vcf.Line, 0, len(cur.Lines)+len(photo))}
	placed := false

	for _, l := range cur.Lines {
		if photoLines[l.Name] {
			if !placed {
				out.Lines = append(out.Lines, photo...)
				placed = true
			}

			continue
		}

		if l.Name == "END" && !placed {
			out.Lines = append(out.Lines, photo...)
			placed = true
		}

		out.Lines = append(out.Lines, l)
	}

	return out
}

// PhotoKey identifies a photo by its bytes, so two revisions with the
// same picture count once.
func (c Card) PhotoKey() string {
	l, ok := c.Get("PHOTO")
	if !ok {
		return ""
	}

	v := strings.Map(dropSpace, l.Value)
	if len(v) > 64 {
		return v[:32] + v[len(v)-32:] + strconv.Itoa(len(v))
	}

	return v
}
