// Package store keeps CardDAV and CalDAV collections in git repositories,
// using the layout of Xandikos so the two servers can share one store:
//
//	<root>/<principal>/contacts/<addressbook>/   (a git repository)
//	<root>/<principal>/calendars/<calendar>/     (a git repository)
//
// Objects are the files at the top of HEAD's tree. An object's ETag is its
// git blob id and a collection's sync token is its HEAD commit id. Writes
// go straight into the object database and move the branch with a
// compare-and-swap, so they cost the same with ten objects or ten thousand.
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// Errors returned by the store.
var (
	ErrNotFound      = errors.New("not found")
	ErrPrecondition  = errors.New("precondition failed")
	ErrExists        = errors.New("already exists")
	ErrInvalidToken  = errors.New("invalid sync token")
	ErrInvalidName   = errors.New("invalid resource name")
	ErrConflict      = errors.New("collection changed during the write, try again")
	ErrCollectionDir = errors.New("collection directory is not a git repository")
)

// Kind is the type of a collection.
type Kind string

// Collection kinds, with the home directory each lives in.
const (
	AddressBook Kind = "addressbook"
	Calendar    Kind = "calendar"
)

// Home returns the directory name under the principal for the kind.
func (k Kind) Home() string {
	if k == AddressBook {
		return "contacts"
	}

	return "calendars"
}

// Author is written on every commit.
type Author struct {
	Name  string
	Email string
}

// Store is the set of collections of one principal.
type Store struct {
	root      string
	principal string
	author    Author

	mu          sync.Mutex
	collections map[string]*Collection // key: "<home>/<name>"
}

// Open opens the principal directory under root and finds its collections.
func Open(root, principal string, author Author) (*Store, error) {
	s := &Store{root: root, principal: principal, author: author, mu: sync.Mutex{}, collections: map[string]*Collection{}}

	if err := s.Rescan(); err != nil {
		return nil, err
	}

	return s, nil
}

// Principal returns the principal name.
func (s *Store) Principal() string { return s.principal }

// Rescan finds collections created outside the server.
func (s *Store) Rescan() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, kind := range []Kind{AddressBook, Calendar} {
		home := filepath.Join(s.root, s.principal, kind.Home())

		entries, err := os.ReadDir(home)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return fmt.Errorf("reading %s: %w", home, err)
		}

		for _, e := range entries {
			key := kind.Home() + "/" + e.Name()
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || s.collections[key] != nil {
				continue
			}

			if _, err := os.Stat(filepath.Join(home, e.Name(), ".git")); err != nil {
				continue
			}

			c, err := openCollection(filepath.Join(home, e.Name()), e.Name(), kind, s.author)
			if err != nil {
				return err
			}

			s.collections[key] = c
		}
	}

	return nil
}

// Collections returns the collections of one kind, sorted by name.
func (s *Store) Collections(kind Kind) []*Collection {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*Collection

	for _, c := range s.collections {
		if c.kind == kind {
			out = append(out, c)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })

	return out
}

// Collection returns one collection.
func (s *Store) Collection(kind Kind, name string) (*Collection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := s.collections[kind.Home()+"/"+name]
	if c == nil {
		return nil, ErrNotFound
	}

	return c, nil
}

// Create makes a new collection: a git repository with a metadata file.
func (s *Store) Create(kind Kind, name string, meta Meta, requester string) (*Collection, error) {
	if !ValidName(name) {
		return nil, ErrInvalidName
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := kind.Home() + "/" + name
	if s.collections[key] != nil {
		return nil, ErrExists
	}

	dir := filepath.Join(s.root, s.principal, kind.Home(), name)
	if _, err := os.Stat(dir); err == nil {
		return nil, ErrExists
	}

	_, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		DefaultBranch: plumbing.NewBranchReferenceName("main"),
		Bare:          false,
	})
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}

	c, err := openCollection(dir, name, kind, s.author)
	if err != nil {
		return nil, err
	}

	meta.Set("", "type", string(kind))

	if err := c.writeMeta(meta, "Created "+name, requester); err != nil {
		return nil, err
	}

	s.collections[key] = c

	return c, nil
}

// EnsureDefaults creates "addressbook" and "calendar" when the principal
// has no collection of that kind yet.
func (s *Store) EnsureDefaults() error {
	defaults := []struct {
		kind Kind
		name string
	}{{AddressBook, "addressbook"}, {Calendar, "calendar"}}

	for _, d := range defaults {
		if len(s.Collections(d.kind)) > 0 {
			continue
		}

		var m Meta

		m.Set("", "displayname", d.name)

		if _, err := s.Create(d.kind, d.name, m, "farvardin init"); err != nil {
			return err
		}
	}

	return nil
}

// ValidName reports whether name is safe as a file or directory name.
func ValidName(name string) bool {
	if name == "" || len(name) > 255 || strings.HasPrefix(name, ".") {
		return false
	}

	return !strings.ContainsAny(name, "/\\\x00") && !slices.Contains([]string{".", ".."}, name)
}
