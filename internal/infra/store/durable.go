package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// DefaultObjectCache is go-git's decoded-object cache per collection.
// go-git's own default is 96 MiB per repository, far too much for a small
// board; objects here are small and read once per request anyway.
const DefaultObjectCache = 8 << 20

// ErrWorktree marks a write whose commit succeeded but whose checked-out
// file or index could not be refreshed; the data is safe in git.
var ErrWorktree = errors.New("committed, but the worktree or index was not updated")

// openRepo opens a non-bare repository with a bounded object cache.
func openRepo(dir string, cacheBytes int64) (*git.Repository, error) {
	if cacheBytes <= 0 {
		cacheBytes = DefaultObjectCache
	}

	wt := osfs.New(dir)

	dot, err := wt.Chroot(git.GitDirName)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", dir, err)
	}

	//nolint:exhaustruct // defaults: no exclusive access, no kept descriptors
	st := filesystem.NewStorageWithOptions(dot, cache.NewObjectLRU(cache.FileSize(cacheBytes)), filesystem.Options{})

	repo, err := git.Open(st, wt)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", dir, err)
	}

	return repo, nil
}

// syncFile flushes a file and the directory entry that names it, so a
// power cut cannot leave a reference pointing at an object that was never
// written to flash.
func syncFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // paths are built from the store
	if errors.Is(err, os.ErrNotExist) {
		return nil // packed or not written as a loose file
	}

	if err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}

	err = f.Sync()
	_ = f.Close()

	if err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}

	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}

	return nil
}

func (c *Collection) syncObjects(hashes ...plumbing.Hash) error {
	for _, h := range hashes {
		s := h.String()
		if err := syncFile(filepath.Join(c.dir, git.GitDirName, "objects", s[:2], s[2:])); err != nil {
			return err
		}
	}

	return nil
}

func (c *Collection) syncRef(name plumbing.ReferenceName) error {
	return syncFile(filepath.Join(c.dir, git.GitDirName, name.String()))
}
