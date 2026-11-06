package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage"
)

// Object is one resource of a collection.
type Object struct {
	Name string
	// ETag is the git blob id, without quotes.
	ETag string
	Size int64
}

// Snapshot is the state of a collection at one commit.
type Snapshot struct {
	// Token is the commit id; "" for an empty repository.
	Token   string
	Objects []Object
	byName  map[string]int
	meta    Meta
}

// Lookup returns the object with that name.
func (s *Snapshot) Lookup(name string) (Object, bool) {
	i, ok := s.byName[name]
	if !ok {
		return Object{}, false
	}

	return s.Objects[i], true
}

// Meta returns the metadata at this snapshot.
func (s *Snapshot) Meta() Meta { return s.meta }

// Condition carries the HTTP preconditions of a write.
type Condition struct {
	// IfMatch is an ETag (without quotes) the current object must have;
	// "*" means it must exist.
	IfMatch string
	// IfNoneMatch is true for "If-None-Match: *": the object must not exist.
	IfNoneMatch bool
}

// Collection is one address book or calendar.
type Collection struct {
	dir    string
	name   string
	kind   Kind
	author Author
	repo   *git.Repository

	mu   sync.Mutex
	last *Snapshot
}

func openCollection(dir, name string, kind Kind, author Author) (*Collection, error) {
	repo, err := git.PlainOpen(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrCollectionDir, dir, err)
	}

	return &Collection{dir: dir, name: name, kind: kind, author: author, repo: repo, mu: sync.Mutex{}, last: nil}, nil
}

// Name returns the collection's directory name.
func (c *Collection) Name() string { return c.name }

// Kind returns the collection kind.
func (c *Collection) Kind() Kind { return c.kind }

// Snapshot returns the objects at HEAD.
func (c *Collection) Snapshot() (*Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.snapshotLocked()
}

func (c *Collection) snapshotLocked() (*Snapshot, error) {
	head, err := c.head()
	if err != nil {
		return nil, err
	}

	if head == nil {
		return &Snapshot{Token: "", Objects: nil, byName: map[string]int{}, meta: Meta{}}, nil
	}

	if c.last != nil && c.last.Token == head.Hash().String() {
		return c.last, nil
	}

	snap, err := c.snapshotAt(head.Hash())
	if err != nil {
		return nil, err
	}

	c.last = snap

	return snap, nil
}

func (c *Collection) snapshotAt(commitHash plumbing.Hash) (*Snapshot, error) {
	commit, err := c.repo.CommitObject(commitHash)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("reading tree of %s: %w", commitHash, err)
	}

	snap := &Snapshot{Token: commitHash.String(), Objects: nil, byName: map[string]int{}, meta: Meta{}}

	for _, e := range tree.Entries {
		if e.Name == MetaFile {
			if data, err := c.blob(e.Hash); err == nil {
				snap.meta = ParseMeta(data)
			}

			continue
		}

		if !e.Mode.IsFile() || strings.HasPrefix(e.Name, ".") {
			continue
		}

		size, _ := c.repo.Storer.EncodedObjectSize(e.Hash)
		snap.byName[e.Name] = len(snap.Objects)
		snap.Objects = append(snap.Objects, Object{Name: e.Name, ETag: e.Hash.String(), Size: size})
	}

	return snap, nil
}

func (c *Collection) head() (*plumbing.Reference, error) {
	ref, err := c.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, nil //nolint:nilnil // an empty repository has no HEAD commit; callers check for nil
	}

	if err != nil {
		return nil, fmt.Errorf("reading HEAD of %s: %w", c.dir, err)
	}

	return ref, nil
}

func (c *Collection) blob(h plumbing.Hash) ([]byte, error) {
	b, err := c.repo.BlobObject(h)
	if err != nil {
		return nil, fmt.Errorf("reading blob %s: %w", h, err)
	}

	r, err := b.Reader()
	if err != nil {
		return nil, fmt.Errorf("reading blob %s: %w", h, err)
	}
	defer func() { _ = r.Close() }()

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading blob %s: %w", h, err)
	}

	return data, nil
}

// Read returns the content and ETag of an object at HEAD.
func (c *Collection) Read(name string) ([]byte, string, error) {
	snap, err := c.Snapshot()
	if err != nil {
		return nil, "", err
	}

	obj, ok := snap.Lookup(name)
	if !ok {
		return nil, "", ErrNotFound
	}

	data, err := c.ReadBlob(obj.ETag)

	return data, obj.ETag, err
}

// ReadBlob returns the content of an object by its ETag (blob id).
func (c *Collection) ReadBlob(etag string) ([]byte, error) {
	return c.blob(plumbing.NewHash(etag))
}

// Meta returns the metadata at HEAD.
func (c *Collection) Meta() (Meta, error) {
	snap, err := c.Snapshot()
	if err != nil {
		return Meta{}, err
	}

	return snap.meta, nil
}

// Changes lists what changed since token: objects added or modified, and
// names deleted. It returns ErrInvalidToken for a token the repository
// does not know.
func (c *Collection) Changes(token string) ([]Object, []string, *Snapshot, error) {
	snap, err := c.Snapshot()
	if err != nil {
		return nil, nil, nil, err
	}

	if token == "" {
		return snap.Objects, nil, snap, nil
	}

	if token == snap.Token {
		return nil, nil, snap, nil
	}

	if !plumbing.IsHash(token) {
		return nil, nil, nil, ErrInvalidToken
	}

	old, err := c.snapshotAt(plumbing.NewHash(token))
	if err != nil {
		return nil, nil, nil, ErrInvalidToken
	}

	changed, deleted := Diff(old, snap)

	return changed, deleted, snap, nil
}

// Put stores data under name. It returns the new ETag and whether the
// object was created.
func (c *Collection) Put(name string, data []byte, cond Condition, requester string) (string, bool, error) {
	if !ValidName(name) {
		return "", false, ErrInvalidName
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	snap, err := c.snapshotLocked()
	if err != nil {
		return "", false, err
	}

	cur, exists := snap.Lookup(name)
	if err := checkCondition(cond, cur, exists); err != nil {
		return "", false, err
	}

	h, err := c.storeBlob(data)
	if err != nil {
		return "", false, err
	}

	if exists && cur.ETag == h.String() {
		return cur.ETag, false, nil
	}

	verb := "Added"
	if exists {
		verb = "Modified"
	}

	err = c.commit(map[string]*plumbing.Hash{name: &h}, verb+" "+name, requester)
	if err != nil {
		return "", false, err
	}

	if err := c.writeWorktree(name, data, h); err != nil {
		return "", false, err
	}

	return h.String(), !exists, nil
}

// Delete removes an object.
func (c *Collection) Delete(name string, cond Condition, requester string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	snap, err := c.snapshotLocked()
	if err != nil {
		return err
	}

	cur, exists := snap.Lookup(name)
	if !exists {
		return ErrNotFound
	}

	if err := checkCondition(cond, cur, exists); err != nil {
		return err
	}

	if err := c.commit(map[string]*plumbing.Hash{name: nil}, "Deleted "+name, requester); err != nil {
		return err
	}

	return c.writeWorktree(name, nil, plumbing.ZeroHash)
}

// SetMeta replaces the collection metadata.
func (c *Collection) SetMeta(meta Meta, requester string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.writeMeta(meta, "Updated collection properties", requester)
}

func (c *Collection) writeMeta(meta Meta, message, requester string) error {
	data := meta.Encode()

	h, err := c.storeBlob(data)
	if err != nil {
		return err
	}

	if err := c.commit(map[string]*plumbing.Hash{MetaFile: &h}, message, requester); err != nil {
		return err
	}

	return c.writeWorktree(MetaFile, data, h)
}

func checkCondition(cond Condition, cur Object, exists bool) error {
	if cond.IfNoneMatch && exists {
		return ErrPrecondition
	}

	switch {
	case cond.IfMatch == "":
	case !exists:
		return ErrPrecondition
	case cond.IfMatch != "*" && cond.IfMatch != cur.ETag:
		return ErrPrecondition
	}

	return nil
}

func (c *Collection) storeBlob(data []byte) (plumbing.Hash, error) {
	obj := c.repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)

	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("writing blob: %w", err)
	}

	if _, err := w.Write(data); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("writing blob: %w", err)
	}

	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("writing blob: %w", err)
	}

	h, err := c.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("storing blob: %w", err)
	}

	return h, nil
}

// commit applies changes (name -> new blob, nil to delete) on top of HEAD
// and moves the branch with a compare-and-swap.
func (c *Collection) commit(changes map[string]*plumbing.Hash, message, requester string) error {
	headRef, err := c.head()
	if err != nil {
		return err
	}

	var (
		entries []object.TreeEntry
		parents []plumbing.Hash
	)

	if headRef != nil {
		commit, err := c.repo.CommitObject(headRef.Hash())
		if err != nil {
			return fmt.Errorf("reading HEAD commit: %w", err)
		}

		tree, err := commit.Tree()
		if err != nil {
			return fmt.Errorf("reading HEAD tree: %w", err)
		}

		entries = append(entries, tree.Entries...)
		parents = []plumbing.Hash{headRef.Hash()}
	}

	for name, h := range changes {
		i := sort.Search(len(entries), func(i int) bool { return entries[i].Name >= name })
		found := i < len(entries) && entries[i].Name == name

		switch {
		case h == nil && found:
			entries = append(entries[:i], entries[i+1:]...)
		case h == nil:
		case found:
			entries[i].Hash, entries[i].Mode = *h, filemode.Regular
		default:
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: *h})
		}
	}

	sortTree(entries)

	treeHash, err := c.encode(&object.Tree{Entries: entries})
	if err != nil {
		return err
	}

	now := time.Now()
	sig := object.Signature{Name: c.author.Name, Email: c.author.Email, When: now}

	if requester != "" {
		message += "\n\nRequester: " + requester
	}

	//nolint:exhaustruct // a plain commit: no signature, no encoding header
	commitHash, err := c.encode(&object.Commit{Author: sig, Committer: sig, Message: message, TreeHash: treeHash, ParentHashes: parents})
	if err != nil {
		return err
	}

	return c.moveHead(headRef, commitHash)
}

// sortTree sorts entries the way git does: directories compare as if
// their name ended in "/".
func sortTree(entries []object.TreeEntry) {
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}

		return e.Name
	}

	sort.Slice(entries, func(i, j int) bool { return key(entries[i]) < key(entries[j]) })
}

type encodable interface {
	Encode(o plumbing.EncodedObject) error
}

func (c *Collection) encode(v encodable) (plumbing.Hash, error) {
	obj := c.repo.Storer.NewEncodedObject()
	if err := v.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encoding object: %w", err)
	}

	h, err := c.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("storing object: %w", err)
	}

	return h, nil
}

func (c *Collection) moveHead(old *plumbing.Reference, commit plumbing.Hash) error {
	branch := plumbing.NewBranchReferenceName("main")

	if headSym, err := c.repo.Storer.Reference(plumbing.HEAD); err == nil && headSym.Type() == plumbing.SymbolicReference {
		branch = headSym.Target()
	}

	var oldBranch *plumbing.Reference
	if old != nil {
		oldBranch = plumbing.NewHashReference(branch, old.Hash())
	}

	if err := c.repo.Storer.CheckAndSetReference(plumbing.NewHashReference(branch, commit), oldBranch); err != nil {
		if errors.Is(err, storage.ErrReferenceHasChanged) {
			return ErrConflict
		}

		return fmt.Errorf("moving %s: %w", branch, err)
	}

	c.last = nil

	return nil
}

// writeWorktree keeps the checked-out files and the index in step with
// HEAD, so `git status` stays clean. data == nil removes the file.
func (c *Collection) writeWorktree(name string, data []byte, h plumbing.Hash) error {
	path := filepath.Join(c.dir, name)

	idx, err := c.repo.Storer.Index()
	if err != nil {
		idx = &index.Index{Version: 2, Entries: nil, Cache: nil, ResolveUndo: nil, EndOfIndexEntry: nil}
	}

	// Any change invalidates the cached-tree extension.
	idx.Cache = nil
	idx.EndOfIndexEntry = nil

	if data == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", path, err)
		}

		_, _ = idx.Remove(name)
	} else {
		tmp := path + ".farvardin-tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // collection files are as readable as the git objects next to them
			return fmt.Errorf("writing %s: %w", path, err)
		}

		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}

		fi, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}

		e, err := idx.Entry(name)
		if err != nil {
			e = idx.Add(name)
		}

		e.Hash, e.Mode, e.Size = h, filemode.Regular, uint32(fi.Size()) //nolint:gosec // vCards and events are far below 4 GiB
		e.ModifiedAt, e.CreatedAt = fi.ModTime(), fi.ModTime()
	}

	if err := c.repo.Storer.SetIndex(idx); err != nil {
		return fmt.Errorf("writing index: %w", err)
	}

	return nil
}

// Revision is one commit that touched an object.
type Revision struct {
	Commit    string
	When      time.Time
	Message   string
	Requester string
	// ETag is the object's blob id after the commit; "" if it was deleted.
	ETag string
}

// History returns up to limit commits that changed name, newest first.
func (c *Collection) History(name string, limit int) ([]Revision, error) {
	head, err := c.head()
	if err != nil || head == nil {
		return nil, err
	}

	//nolint:exhaustruct // only the start and the path filter are needed
	iter, err := c.repo.Log(&git.LogOptions{From: head.Hash(), FileName: &name})
	if err != nil {
		return nil, fmt.Errorf("reading history: %w", err)
	}
	defer iter.Close()

	var out []Revision

	err = iter.ForEach(func(cm *object.Commit) error {
		subject, body, _ := strings.Cut(cm.Message, "\n")
		rev := Revision{Commit: cm.Hash.String(), When: cm.Author.When, Message: subject, Requester: "", ETag: ""}

		for line := range strings.SplitSeq(body, "\n") {
			if r, ok := strings.CutPrefix(line, "Requester: "); ok {
				rev.Requester = r
			}
		}

		if f, err := cm.File(name); err == nil {
			rev.ETag = f.Hash.String()
		}

		out = append(out, rev)
		if len(out) >= limit {
			return storer.ErrStop
		}

		return nil
	})
	if err != nil {
		return out, fmt.Errorf("reading history: %w", err)
	}

	return out, nil
}

// Recent returns the newest commits of the collection.
func (c *Collection) Recent(limit int) ([]Revision, error) {
	head, err := c.head()
	if err != nil || head == nil {
		return nil, err
	}

	//nolint:exhaustruct // only the start is needed
	iter, err := c.repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return nil, fmt.Errorf("reading history: %w", err)
	}
	defer iter.Close()

	var out []Revision

	err = iter.ForEach(func(cm *object.Commit) error {
		subject, body, _ := strings.Cut(cm.Message, "\n")
		rev := Revision{Commit: cm.Hash.String(), When: cm.Author.When, Message: subject, Requester: "", ETag: ""}

		for line := range strings.SplitSeq(body, "\n") {
			if r, ok := strings.CutPrefix(line, "Requester: "); ok {
				rev.Requester = r
			}
		}

		out = append(out, rev)
		if len(out) >= limit {
			return storer.ErrStop
		}

		return nil
	})
	if err != nil {
		return out, fmt.Errorf("reading history: %w", err)
	}

	return out, nil
}

// SnapshotAt returns the objects at an earlier sync token.
func (c *Collection) SnapshotAt(token string) (*Snapshot, error) {
	if !plumbing.IsHash(token) {
		return nil, ErrInvalidToken
	}

	return c.snapshotAt(plumbing.NewHash(token))
}

// NewSnapshot builds a snapshot for a collection that is not stored in
// git, such as the generated birthdays calendar.
func NewSnapshot(token string, objects []Object, meta Meta) *Snapshot {
	s := &Snapshot{Token: token, Objects: objects, byName: make(map[string]int, len(objects)), meta: meta}

	for i, o := range objects {
		s.byName[o.Name] = i
	}

	return s
}

// Diff lists what changed between two snapshots of one collection.
func Diff(old, cur *Snapshot) ([]Object, []string) {
	var (
		changed []Object
		deleted []string
	)

	for _, o := range cur.Objects {
		if prev, ok := old.Lookup(o.Name); !ok || prev.ETag != o.ETag {
			changed = append(changed, o)
		}
	}

	for _, o := range old.Objects {
		if _, ok := cur.Lookup(o.Name); !ok {
			deleted = append(deleted, o.Name)
		}
	}

	return changed, deleted
}
