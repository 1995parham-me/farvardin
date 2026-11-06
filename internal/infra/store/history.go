package store

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// historyPerFile bounds the revisions kept per object.
const historyPerFile = 50

// history maps each object name to the commits that changed it, newest
// first. It is built by one walk of the first-parent history and then
// extended with only the commits added since, so a contact page never
// walks git again. go-git's path-filtered log diffs whole trees per commit
// and took seconds per page on a Raspberry Pi.
type history struct {
	mu     sync.Mutex
	upTo   plumbing.Hash // newest commit already indexed
	byName map[string][]Revision
}

// IndexHistory brings the history index up to HEAD. It is cheap when
// nothing changed; the server calls it in the background at start.
func (c *Collection) IndexHistory() error {
	c.hist.mu.Lock()
	defer c.hist.mu.Unlock()

	head, err := c.head()
	if err != nil || head == nil {
		return err
	}

	if head.Hash() == c.hist.upTo {
		return nil
	}

	var newer []struct {
		name string
		rev  Revision
	}

	cur, err := c.repo.CommitObject(head.Hash())
	if err != nil {
		return fmt.Errorf("reading %s: %w", head.Hash(), err)
	}

	curTree, err := cur.Tree()
	if err != nil {
		return fmt.Errorf("reading tree of %s: %w", cur.Hash, err)
	}

	for cur != nil && cur.Hash != c.hist.upTo {
		var (
			parent     *object.Commit
			parentTree *object.Tree
		)

		if cur.NumParents() > 0 {
			parent, err = cur.Parent(0)
			if err != nil {
				return fmt.Errorf("reading parent of %s: %w", cur.Hash, err)
			}

			parentTree, err = parent.Tree()
			if err != nil {
				return fmt.Errorf("reading tree of %s: %w", parent.Hash, err)
			}
		}

		for _, ch := range changedEntries(parentTree, curTree) {
			rev := revisionOf(cur)
			rev.ETag = ch.etag
			newer = append(newer, struct {
				name string
				rev  Revision
			}{ch.name, rev})
		}

		cur, curTree = parent, parentTree
	}

	if c.hist.byName == nil {
		c.hist.byName = map[string][]Revision{}
	}

	// newer is newest first; prepend it to what was indexed before.
	add := map[string][]Revision{}
	for _, n := range newer {
		add[n.name] = append(add[n.name], n.rev)
	}

	for name, revs := range add {
		merged := slices.Concat(revs, c.hist.byName[name])
		c.hist.byName[name] = merged[:min(len(merged), historyPerFile)]
	}

	c.hist.upTo = head.Hash()

	return nil
}

type change struct {
	name string
	etag string // "" when deleted
}

// changedEntries compares two flat trees (parent may be nil) entry by
// entry; both are sorted by name, so this is one linear merge.
func changedEntries(parent, cur *object.Tree) []change {
	var (
		a   []object.TreeEntry
		out []change
	)

	if parent != nil {
		a = parent.Entries
	}

	b := cur.Entries
	i, j := 0, 0

	for i < len(a) || j < len(b) {
		switch {
		case j >= len(b) || (i < len(a) && a[i].Name < b[j].Name):
			out = append(out, change{name: a[i].Name, etag: ""})
			i++
		case i >= len(a) || b[j].Name < a[i].Name:
			out = append(out, change{name: b[j].Name, etag: b[j].Hash.String()})
			j++
		default:
			if a[i].Hash != b[j].Hash {
				out = append(out, change{name: b[j].Name, etag: b[j].Hash.String()})
			}

			i++
			j++
		}
	}

	return out
}

func revisionOf(cm *object.Commit) Revision {
	subject, body, _ := strings.Cut(cm.Message, "\n")
	rev := Revision{Commit: cm.Hash.String(), When: cm.Author.When, Message: subject, Requester: "", ETag: ""}

	for line := range strings.SplitSeq(body, "\n") {
		if r, ok := strings.CutPrefix(line, "Requester: "); ok {
			rev.Requester = r
		}
	}

	return rev
}

// History returns up to limit commits that changed name, newest first.
func (c *Collection) History(name string, limit int) ([]Revision, error) {
	if err := c.IndexHistory(); err != nil {
		return nil, err
	}

	c.hist.mu.Lock()
	defer c.hist.mu.Unlock()

	revs := c.hist.byName[name]

	return append([]Revision(nil), revs[:min(len(revs), limit)]...), nil
}

// Recent returns the newest commits of the collection.
func (c *Collection) Recent(limit int) ([]Revision, error) {
	head, err := c.head()
	if err != nil || head == nil {
		return nil, err
	}

	var out []Revision

	cm, err := c.repo.CommitObject(head.Hash())

	for err == nil && cm != nil && len(out) < limit {
		out = append(out, revisionOf(cm))

		if cm.NumParents() == 0 {
			break
		}

		cm, err = cm.Parent(0)
	}

	if err != nil {
		return out, fmt.Errorf("reading history: %w", err)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].When.After(out[j].When) })

	return out, nil
}
