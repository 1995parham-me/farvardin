// Package edits holds the changes the web UI and MCP make to an address
// book beyond a single field update: merging duplicate cards, restoring a
// card's earlier version or a deleted card, and fixing every phone number
// at once. Each goes through the server's validated write path, so it is
// checked, committed and pushed like a phone's write.
package edits

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/1995parham-me/farvardin/internal/domain/calendar"
	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

// Errors for requests that cannot be carried out.
var (
	ErrNotFound   = errors.New("not found")
	ErrChanged    = errors.New("the card changed since it was read; read it again and retry")
	ErrNotHistory = errors.New("that version is not in this card's history")
	ErrBadMerge   = errors.New("a merge needs one card to keep and at least one other")
)

// Parser returns a parsed object; the DAV handler's caches implement it.
type Parser func(col *store.Collection, etag string) (*contact.Card, *calendar.Component, error)

// Editor makes the changes.
type Editor struct {
	Store *store.Store
	// Parse returns slim cards (no photo bytes); Full keeps the photo.
	Parse, Full Parser
	// Write stores a card through the validated write path; ifMatch ""
	// creates.
	Write func(book, name string, data []byte, ifMatch, requester string) (string, error)
	// Delete removes an object, matching ifMatch.
	Delete func(kind store.Kind, col, name, ifMatch, requester string) error
	// Regions read phone numbers written without a country code.
	Regions []string
}

func (e Editor) book(name string) (*store.Collection, *store.Snapshot, error) {
	col, err := e.Store.Collection(store.AddressBook, name)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: address book %q", ErrNotFound, name)
	}

	snap, err := col.Snapshot()
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", name, err)
	}

	return col, snap, nil
}

// current returns an object of the snapshot, checking that it still has
// the ETag the caller read ("" skips the check).
func current(snap *store.Snapshot, name, etag string) (store.Object, error) {
	obj, ok := snap.Lookup(name)
	if !ok {
		return store.Object{}, fmt.Errorf("%w: contact %q", ErrNotFound, name)
	}

	if etag != "" && etag != obj.ETag {
		return store.Object{}, fmt.Errorf("%s: %w", name, ErrChanged)
	}

	return obj, nil
}

// Duplicates groups the cards of an address book that share a number, an
// email address or a name. Groups marked not the same are left out; the
// count of marked groups is returned with them.
func (e Editor) Duplicates(bookName string) ([]contact.DuplicateGroup, int, error) {
	notSameMu.Lock()
	defer notSameMu.Unlock()

	groups, ns, err := e.duplicates(bookName)
	if err != nil {
		return nil, 0, err
	}

	shown := slices.DeleteFunc(groups, func(g contact.DuplicateGroup) bool { return ns.hidden(bookName, g) })

	return shown, len(ns[bookName]), nil
}

// duplicates is every duplicate group of a book, with the marked sets;
// the caller holds notSameMu.
func (e Editor) duplicates(bookName string) ([]contact.DuplicateGroup, notSame, error) {
	col, snap, err := e.book(bookName)
	if err != nil {
		return nil, nil, err
	}

	ns, err := e.readNotSame()
	if err != nil {
		return nil, nil, err
	}

	entries := make([]contact.Entry, 0, len(snap.Objects))

	for _, o := range snap.Objects {
		if c, _, err := e.Parse(col, o.ETag); err == nil && c != nil {
			entries = append(entries, contact.Entry{ID: o.Name, Card: *c})
		}
	}

	return contact.Duplicates(entries, e.Regions), ns, nil
}

// Card is a card named with the ETag it was read at.
type Card struct {
	ID   string
	ETag string
}

// MergeResult is the card that was kept and the cards merged into it.
type MergeResult struct {
	Kept   string
	ETag   string
	Merged []string
	// Failed names the cards that were merged but could not be deleted
	// afterwards; their content is in the kept card.
	Failed map[string]string
}

// Merge adds what only the other cards have to keep (phone numbers,
// emails, links, the larger photo, and so on; see contact.Merge), stores
// it, and then deletes the others. Every card must still have the ETag
// the caller read, so a phone's edit in between is never lost.
func (e Editor) Merge(bookName string, keep Card, others []Card, requester string) (MergeResult, error) {
	res := MergeResult{Kept: keep.ID, ETag: "", Merged: nil, Failed: nil}

	if keep.ID == "" || len(others) == 0 || slices.ContainsFunc(others, func(o Card) bool { return o.ID == keep.ID }) {
		return res, ErrBadMerge
	}

	col, snap, err := e.book(bookName)
	if err != nil {
		return res, err
	}

	kobj, err := current(snap, keep.ID, keep.ETag)
	if err != nil {
		return res, err
	}

	merged, _, err := e.Full(col, kobj.ETag)
	if err != nil || merged == nil {
		return res, fmt.Errorf("reading %s: %w", keep.ID, err)
	}

	out := *merged
	etags := map[string]string{}

	for _, o := range others {
		obj, err := current(snap, o.ID, o.ETag)
		if err != nil {
			return res, err
		}

		other, _, err := e.Full(col, obj.ETag)
		if err != nil || other == nil {
			return res, fmt.Errorf("reading %s: %w", o.ID, err)
		}

		out = contact.Merge(out, *other, e.Regions)
		etags[o.ID] = obj.ETag
	}

	out.Touch(time.Now())

	res.ETag, err = e.Write(bookName, keep.ID, out.Encode(), kobj.ETag, requester)
	if err != nil {
		return res, err
	}

	for _, o := range others {
		if err := e.Delete(store.AddressBook, bookName, o.ID, etags[o.ID], requester); err != nil {
			if res.Failed == nil {
				res.Failed = map[string]string{}
			}

			res.Failed[o.ID] = err.Error()

			continue
		}

		res.Merged = append(res.Merged, o.ID)
	}

	return res, nil
}

// RestoreVersion puts back a card exactly as it was at an earlier
// revision (a blob id from its history), as a new change on top.
func (e Editor) RestoreVersion(bookName, name, revision, ifMatch, requester string) (string, error) {
	col, snap, err := e.book(bookName)
	if err != nil {
		return "", err
	}

	obj, err := current(snap, name, ifMatch)
	if err != nil {
		return "", err
	}

	if !inHistory(col, name, revision) {
		return "", ErrNotHistory
	}

	data, err := col.ReadBlob(revision)
	if err != nil {
		return "", fmt.Errorf("reading revision %s: %w", revision, err)
	}

	return e.Write(bookName, name, data, obj.ETag, requester)
}

// Deleted lists the deleted cards of an address book, newest first, with
// the name each card had.
func (e Editor) Deleted(bookName string) ([]Removed, error) {
	col, snap, err := e.book(bookName)
	if err != nil {
		return nil, err
	}

	gone, err := col.Deleted()
	if err != nil {
		return nil, fmt.Errorf("reading history: %w", err)
	}

	// A card renamed to another file (a client that names files by UID)
	// or imported again under a new UID is not gone: its UID or its name
	// is still in the book.
	live := map[string]bool{}

	for _, o := range snap.Objects {
		if c, _, err := e.Parse(col, o.ETag); err == nil && c != nil {
			live["uid "+c.UID()] = true
			live["name "+strings.ToLower(strings.TrimSpace(c.DisplayName()))] = true
		}
	}

	out := make([]Removed, 0, len(gone))

	for _, g := range gone {
		// Files that were never cards (a script, a state file) are left out.
		c, _, err := e.Parse(col, g.Last)
		if err != nil || c == nil || live["uid "+c.UID()] || live["name "+strings.ToLower(strings.TrimSpace(c.DisplayName()))] {
			continue
		}

		out = append(out, Removed{Removed: g, Display: c.DisplayName()})
	}

	return out, nil
}

// Removed is a deleted card with the name it showed.
type Removed struct {
	store.Removed

	Display string
}

// RestoreDeleted brings a deleted card back under its old name, with the
// content it had before the delete.
func (e Editor) RestoreDeleted(bookName, name, requester string) (string, error) {
	col, snap, err := e.book(bookName)
	if err != nil {
		return "", err
	}

	if _, ok := snap.Lookup(name); ok {
		return "", fmt.Errorf("%w: %s exists; it is not deleted", ErrBadRestore, name)
	}

	gone, err := col.Deleted()
	if err != nil {
		return "", fmt.Errorf("reading history: %w", err)
	}

	i := slices.IndexFunc(gone, func(r store.Removed) bool { return r.Name == name })
	if i < 0 {
		return "", fmt.Errorf("%w: no deleted contact %q", ErrNotFound, name)
	}

	data, err := col.ReadBlob(gone[i].Last)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", gone[i].Last, err)
	}

	return e.Write(bookName, name, data, "", requester)
}

// ErrBadRestore is returned for a restore of a card that is not deleted.
var ErrBadRestore = errors.New("cannot restore")

// PhoneFixes is what FixAllPhones did to one card.
type PhoneFixes struct {
	ID    string             `json:"id"`
	Name  string             `json:"name"`
	Fixed []contact.PhoneFix `json:"fixed,omitempty"`
	Error string             `json:"error,omitempty"`
}

// FixAllPhones writes every number without a country code in E.164, in
// every card of the address book that has one. Each card is its own
// change; a card that fails (a phone edited it a moment before) is
// reported and the others are still fixed.
func (e Editor) FixAllPhones(bookName, requester string) ([]PhoneFixes, error) {
	col, snap, err := e.book(bookName)
	if err != nil {
		return nil, err
	}

	out := []PhoneFixes{}

	for _, o := range snap.Objects {
		slim, _, err := e.Parse(col, o.ETag)
		if err != nil || slim == nil {
			continue
		}

		if _, fixes := contact.FixPhones(*slim, e.Regions); len(fixes) == 0 {
			continue
		}

		res := PhoneFixes{ID: o.Name, Name: slim.DisplayName(), Fixed: nil, Error: ""}

		full, _, err := e.Full(col, o.ETag)
		if err != nil || full == nil {
			res.Error = fmt.Sprintf("reading the card: %v", err)
			out = append(out, res)

			continue
		}

		fixed, fixes := contact.FixPhones(*full, e.Regions)

		if _, err := e.Write(bookName, o.Name, fixed.Encode(), o.ETag, requester); err != nil {
			res.Error = err.Error()
		} else {
			res.Fixed = fixes
		}

		out = append(out, res)
	}

	return out, nil
}

func inHistory(col *store.Collection, name, etag string) bool {
	if etag == "" {
		return false
	}

	revs, err := col.History(name, 200)
	if err != nil {
		return false
	}

	return slices.ContainsFunc(revs, func(r store.Revision) bool { return r.ETag == etag })
}
