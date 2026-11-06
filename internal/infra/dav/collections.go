package dav

import (
	"crypto/sha1" //nolint:gosec // ETags of generated objects use the same digest as git blob ids; not a security use
	"encoding/hex"
	"maps"
	"sort"
	"strings"
	"sync"

	"github.com/1995parham-me/farvardin/internal/domain/calendar"
	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

// collection is what the protocol layer needs from a collection, whether
// it lives in git or is generated.
type collection interface {
	Name() string
	Kind() store.Kind
	Snapshot() (*store.Snapshot, error)
	SnapshotAt(token string) (*store.Snapshot, error)
	ReadBlob(etag string) ([]byte, error)
}

// writable is a collection clients may change.
type writable interface {
	collection
	Put(name string, data []byte, cond store.Condition, requester string) (string, bool, error)
	Delete(name string, cond store.Condition, requester string) error
	SetMeta(meta store.Meta, requester string) error
}

// parsed is a cached, parsed object.
type parsed struct {
	card *contact.Card
	cal  *calendar.Component
	err  error
}

// cache keeps parsed objects by blob id. Blob ids are content hashes, so
// entries never go stale; the cache is only bounded in size.
type cache struct {
	mu      sync.Mutex
	entries map[string]parsed
	max     int
}

func newCache(maxEntries int) *cache {
	return &cache{mu: sync.Mutex{}, entries: map[string]parsed{}, max: maxEntries}
}

func (c *cache) get(col collection, etag string) parsed {
	c.mu.Lock()
	p, ok := c.entries[etag]
	c.mu.Unlock()

	if ok {
		return p
	}

	data, err := col.ReadBlob(etag)
	if err != nil {
		return parsed{card: nil, cal: nil, err: err}
	}

	p = parse(col.Kind(), data)

	c.mu.Lock()
	if len(c.entries) >= c.max {
		clear(c.entries)
	}

	c.entries[etag] = p
	c.mu.Unlock()

	return p
}

func parse(kind store.Kind, data []byte) parsed {
	if kind == store.AddressBook {
		card, err := contact.Parse(data)
		if err != nil {
			return parsed{card: nil, cal: nil, err: err}
		}

		return parsed{card: &card, cal: nil, err: nil}
	}

	cal, err := calendar.Parse(data)

	return parsed{card: nil, cal: cal, err: err}
}

// uidOwner returns the name of the object in snap whose UID is uid,
// ignoring the object called except.
func (c *cache) uidOwner(col collection, snap *store.Snapshot, uid, except string) string {
	for _, o := range snap.Objects {
		if o.Name == except {
			continue
		}

		p := c.get(col, o.ETag)

		switch {
		case p.card != nil && p.card.UID() == uid:
			return o.Name
		case p.cal != nil:
			for _, m := range p.cal.Main() {
				if m.Value("UID") == uid {
					return o.Name
				}
			}
		}
	}

	return ""
}

// birthdays is the read-only calendar generated from an address book.
type birthdays struct {
	book  *store.Collection
	cache *cache
	opts  calendar.BirthdayOptions
	color string

	mu    sync.Mutex
	gen   map[string]*store.Snapshot // address book token -> generated snapshot
	blobs map[string][]byte          // etag -> data
}

const birthdaysName = "birthdays"

func newBirthdays(book *store.Collection, c *cache, opts calendar.BirthdayOptions, color string) *birthdays {
	return &birthdays{book: book, cache: c, opts: opts, color: color, mu: sync.Mutex{}, gen: map[string]*store.Snapshot{}, blobs: map[string][]byte{}}
}

// Name is the collection name.
func (b *birthdays) Name() string { return birthdaysName }

// Kind is always Calendar.
func (b *birthdays) Kind() store.Kind { return store.Calendar }

// Snapshot generates the events for the address book's HEAD.
func (b *birthdays) Snapshot() (*store.Snapshot, error) {
	src, err := b.book.Snapshot()
	if err != nil {
		return nil, err
	}

	return b.generate(src), nil
}

// SnapshotAt generates the events for an earlier address book commit.
func (b *birthdays) SnapshotAt(token string) (*store.Snapshot, error) {
	src, err := b.book.SnapshotAt(token)
	if err != nil {
		return nil, err
	}

	return b.generate(src), nil
}

// ReadBlob returns a generated event by its ETag.
func (b *birthdays) ReadBlob(etag string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	data, ok := b.blobs[etag]
	if !ok {
		return nil, store.ErrNotFound
	}

	return data, nil
}

func (b *birthdays) generate(src *store.Snapshot) *store.Snapshot {
	b.mu.Lock()
	if s, ok := b.gen[src.Token]; ok {
		b.mu.Unlock()

		return s
	}
	b.mu.Unlock()

	var objects []store.Object

	blobs := map[string][]byte{}

	for _, o := range src.Objects {
		p := b.cache.get(b.book, o.ETag)
		if p.card == nil {
			continue
		}

		for _, ev := range calendar.Birthdays(o.Name, *p.card, b.opts) {
			sum := sha1.Sum(ev.Data) //nolint:gosec // see import
			etag := hex.EncodeToString(sum[:])
			blobs[etag] = ev.Data
			objects = append(objects, store.Object{Name: ev.Name, ETag: etag, Size: int64(len(ev.Data))})
		}
	}

	sort.Slice(objects, func(i, j int) bool { return objects[i].Name < objects[j].Name })

	var meta store.Meta

	meta.Set("", "type", "calendar")
	meta.Set("", "displayname", "Birthdays")
	meta.Set("", "description", "Birthdays and anniversaries from the address book "+b.book.Name())
	meta.Set("", "color", b.color)

	snap := store.NewSnapshot(src.Token, objects, meta)

	b.mu.Lock()
	defer b.mu.Unlock()

	// Keep the newest few generations; sync clients ask for recent tokens.
	if len(b.gen) > 8 {
		clear(b.gen)
	}

	b.gen[src.Token] = snap

	maps.Copy(b.blobs, blobs)

	if len(b.blobs) > 20000 {
		b.blobs = blobs
	}

	return snap
}

// changes lists what changed in col since token.
func changes(col collection, token string) ([]store.Object, []string, *store.Snapshot, error) {
	cur, err := col.Snapshot()
	if err != nil {
		return nil, nil, nil, err
	}

	if token == "" {
		return cur.Objects, nil, cur, nil
	}

	if token == cur.Token {
		return nil, nil, cur, nil
	}

	old, err := col.SnapshotAt(token)
	if err != nil {
		return nil, nil, nil, store.ErrInvalidToken
	}

	changed, deleted := store.Diff(old, cur)

	return changed, deleted, cur, nil
}

// contentType is the media type of objects in a collection.
func contentType(kind store.Kind, p parsed) string {
	if kind == store.AddressBook {
		return "text/vcard; charset=utf-8"
	}

	if p.cal != nil {
		if main := p.cal.Main(); len(main) > 0 {
			return "text/calendar; charset=utf-8; component=" + strings.ToLower(main[0].Name)
		}
	}

	return "text/calendar; charset=utf-8"
}
