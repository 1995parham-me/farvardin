package edits

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
)

// ErrNotGroup is returned when cards marked as not the same are not one
// duplicate group.
var ErrNotGroup = errors.New("these cards are not a duplicate group")

// The groups marked "not the same" live in one JSON file per store, read
// and written under one lock: the web UI and MCP each have an Editor.
var notSameMu sync.Mutex //nolint:gochecknoglobals // one lock for the file both front ends write

const notSameFile = "not-duplicates.json"

// notSame maps an address book to the sets of card ids marked as not the
// same contact.
type notSame map[string][][]string

func (e Editor) readNotSame() (notSame, error) {
	data, err := os.ReadFile(e.Store.StatePath(notSameFile))
	if errors.Is(err, fs.ErrNotExist) {
		return notSame{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", notSameFile, err)
	}

	out := notSame{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("reading %s: %w", notSameFile, err)
	}

	return out, nil
}

func (e Editor) writeNotSame(ns notSame) error {
	path := e.Store.StatePath(notSameFile)

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("writing %s: %w", notSameFile, err)
	}

	data, err := json.MarshalIndent(ns, "", "  ")
	if err != nil {
		return fmt.Errorf("writing %s: %w", notSameFile, err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", notSameFile, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("writing %s: %w", notSameFile, err)
	}

	return nil
}

// hidden reports whether a group was marked not the same: its cards are
// all in one marked set. A card that joins the group later shows it again.
func (ns notSame) hidden(book string, g contact.DuplicateGroup) bool {
	return slices.ContainsFunc(ns[book], func(set []string) bool {
		return !slices.ContainsFunc(g.IDs, func(id string) bool { return !slices.Contains(set, id) })
	})
}

// NotSame marks a duplicate group as different contacts, so Duplicates
// no longer lists it. ids must be a group Duplicates reports now.
func (e Editor) NotSame(bookName string, ids []string) error {
	notSameMu.Lock()
	defer notSameMu.Unlock()

	groups, _, err := e.duplicates(bookName)
	if err != nil {
		return err
	}

	set := slices.Clone(ids)
	slices.Sort(set)

	if !slices.ContainsFunc(groups, func(g contact.DuplicateGroup) bool {
		got := slices.Clone(g.IDs)
		slices.Sort(got)

		return slices.Equal(got, set)
	}) {
		return fmt.Errorf("%w: %v", ErrNotGroup, ids)
	}

	ns, err := e.readNotSame()
	if err != nil {
		return err
	}

	ns[bookName] = append(ns[bookName], set)

	return e.writeNotSame(ns)
}

// ShowAllDuplicates forgets the groups of a book marked not the same.
func (e Editor) ShowAllDuplicates(bookName string) error {
	notSameMu.Lock()
	defer notSameMu.Unlock()

	ns, err := e.readNotSame()
	if err != nil {
		return err
	}

	if _, ok := ns[bookName]; !ok {
		return nil
	}

	delete(ns, bookName)

	return e.writeNotSame(ns)
}
