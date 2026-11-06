package web

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/1995parham-me/farvardin/internal/infra/store"
)

// assetVersion changes the static URLs on every build, so browsers can
// cache them forever.
var assetVersion = time.Now().UTC().Format("20060102150405")

func cacheForever(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		h.ServeHTTP(w, r)
	})
}

type thumb struct {
	data  []byte
	media string
}

// thumbCache keeps scaled photos by "<blob id>-<size>". Blob ids are
// content hashes, so entries never go stale; the cache is only bounded.
type thumbCache struct {
	mu    sync.Mutex
	items map[string]thumb
	bytes int
	max   int
}

func newThumbCache(maxBytes int) *thumbCache {
	return &thumbCache{mu: sync.Mutex{}, items: map[string]thumb{}, bytes: 0, max: maxBytes}
}

func (c *thumbCache) get(key string) (thumb, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	t, ok := c.items[key]

	return t, ok
}

func (c *thumbCache) put(key string, t thumb) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.bytes+len(t.data) > c.max {
		clear(c.items)
		c.bytes = 0
	}

	c.items[key] = t
	c.bytes += len(t.data)
}

// listCache keeps the rendered rows of a contact list until the address
// book changes (its sync token moves).
type listCache struct {
	mu    sync.Mutex
	items map[string]listEntry
}

type listEntry struct {
	token  string
	people []person
}

func newListCache() *listCache { return &listCache{mu: sync.Mutex{}, items: map[string]listEntry{}} }

func (c *listCache) get(book, token string) ([]person, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.items[book]

	return e.people, ok && e.token == token
}

func (c *listCache) put(book, token string, people []person) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items[book] = listEntry{token: token, people: people}
}

// AvatarSize is the thumbnail size the contact list uses.
const AvatarSize = 96

// Warm fills the caches in the background: the history index of every
// collection, the contact lists and every avatar thumbnail. On a
// Raspberry Pi a cold avatar costs ~70 ms (JPEG decode + scale), so a
// cold contact list would cost a minute of CPU; warmed, it costs nothing.
func (h *Handler) Warm(ctx context.Context) {
	start := time.Now()

	for _, kind := range []store.Kind{store.AddressBook, store.Calendar} {
		for _, c := range h.store.Collections(kind) {
			if err := c.IndexHistory(); err != nil {
				h.log.Warn("indexing history", "collection", c.Name(), "error", err)
			}
		}
	}

	h.log.Info("history indexed", "took", time.Since(start).Round(time.Millisecond))

	start = time.Now()
	n := 0

	for _, book := range h.store.Collections(store.AddressBook) {
		people, snap, err := h.people(book)
		if err != nil {
			continue
		}

		for _, p := range people {
			if ctx.Err() != nil {
				return
			}

			if !p.HasPhoto {
				continue
			}

			if obj, ok := snap.Lookup(p.File); ok {
				if _, _, ok := h.thumbnail(book, obj, AvatarSize); ok {
					n++
				}
			}
		}
	}

	h.log.Info("avatars warmed", "count", n, "took", time.Since(start).Round(time.Millisecond))
}
