package dav

import (
	"errors"
	"fmt"
	"strings"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

// ErrInvalidCard is returned by WriteCard for a card that fails
// validation.
var ErrInvalidCard = errors.New("invalid card")

// WriteCard stores a card on behalf of the web UI, through the same checks
// a client write gets: it must parse, validate, keep its UID unique, and
// match ifMatch (the ETag it was edited from). Subscribers are pushed.
// The photo policy is not applied: the UI writes a photo on purpose.
func (h *Handler) WriteCard(book, name string, data []byte, ifMatch, requester string) (string, error) {
	col, err := h.store.Collection(store.AddressBook, book)
	if err != nil {
		return "", fmt.Errorf("address book %s: %w", book, err)
	}

	card, err := contact.Parse(data)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidCard, err)
	}

	if issues := contact.Validate(card); contact.HasErrors(issues) {
		var msgs []string

		for _, i := range issues {
			if i.Severity == contact.Error {
				msgs = append(msgs, i.Message)
			}
		}

		return "", fmt.Errorf("%w: %s", ErrInvalidCard, strings.Join(msgs, "; "))
	}

	snap, err := col.Snapshot()
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", book, err)
	}

	if owner := h.cache.uidOwner(col, snap, card.UID(), name); owner != "" {
		return "", fmt.Errorf("%w: UID already used by %s", ErrInvalidCard, owner)
	}

	etag, _, err := col.Put(name, data, store.Condition{IfMatch: ifMatch, IfNoneMatch: ifMatch == ""}, requester)
	if errors.Is(err, store.ErrWorktree) {
		h.log.Warn("write committed; worktree not refreshed", "object", name, "error", err)
	} else if err != nil {
		return "", fmt.Errorf("storing %s: %w", name, err)
	}

	h.metrics.Write(string(store.AddressBook), "modified")
	h.pushChange(col)

	return etag, nil
}
