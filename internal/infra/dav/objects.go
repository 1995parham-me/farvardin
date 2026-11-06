package dav

import (
	"errors"
	"fmt"

	"github.com/1995parham-me/farvardin/internal/domain/calendar"
	"github.com/1995parham-me/farvardin/internal/infra/push"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

// ErrInvalidEvent is returned for an event that fails validation.
var ErrInvalidEvent = errors.New("invalid event")

// WriteEvent stores an event through the checks a client write gets.
// ifMatch "" with create true requires the name to be free.
func (h *Handler) WriteEvent(cal, name string, data []byte, ifMatch string, create bool, requester string) (string, error) {
	col, err := h.store.Collection(store.Calendar, cal)
	if err != nil {
		return "", fmt.Errorf("calendar %s: %w", cal, err)
	}

	parsed, err := calendar.Parse(data)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}

	snap, err := col.Snapshot()
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", cal, err)
	}

	uid, err := calendar.Validate(parsed, supportedComponents(snap.Meta()))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}

	if owner := h.cache.uidOwner(col, snap, uid, name); owner != "" {
		return "", fmt.Errorf("%w: UID already used by %s", ErrInvalidEvent, owner)
	}

	etag, _, err := col.Put(name, data, store.Condition{IfMatch: ifMatch, IfNoneMatch: create}, requester)
	if errors.Is(err, store.ErrWorktree) {
		h.log.Warn("write committed; worktree not refreshed", "object", name, "error", err)
	} else if err != nil {
		return "", fmt.Errorf("storing %s: %w", name, err)
	}

	h.metrics.Write(string(store.Calendar), "modified")
	h.pushChange(col)

	return etag, nil
}

// DeleteObject removes an object from a collection, with If-Match.
func (h *Handler) DeleteObject(kind store.Kind, colName, name, ifMatch, requester string) error {
	col, err := h.store.Collection(kind, colName)
	if err != nil {
		return fmt.Errorf("collection %s: %w", colName, err)
	}

	err = col.Delete(name, store.Condition{IfMatch: ifMatch, IfNoneMatch: false}, requester)
	if errors.Is(err, store.ErrWorktree) {
		h.log.Warn("delete committed; worktree not refreshed", "object", name, "error", err)
	} else if err != nil {
		return fmt.Errorf("deleting %s: %w", name, err)
	}

	h.metrics.Write(string(kind), "deleted")
	h.pushChange(col)

	return nil
}

func (h *Handler) pushChange(col *store.Collection) {
	if h.opts.Push == nil {
		return
	}

	if after, err := col.Snapshot(); err == nil {
		h.opts.Push.Notify(col.Dir(), push.Change{SyncToken: syncToken(after), Properties: nil, SkipAll: false, Skip: nil})
	}
}
