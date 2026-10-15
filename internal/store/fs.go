package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/delefrati/xylem/internal/hash"
)

// FS stores objects as files under a root directory, sharded as
// <root>/objects/ab/cd/<hex>. Writes go to a temp file in the target directory
// that is fsynced and then renamed into place, so readers never see a partial
// object.
type FS struct {
	root string
}

// NewFS returns a store rooted at dir. Directories are created on first Put.
func NewFS(dir string) *FS {
	return &FS{root: dir}
}

func (s *FS) path(id hash.ID) string {
	return filepath.Join(s.root, filepath.FromSlash(Key(id)))
}

// Get implements ObjectStore.
func (s *FS) Get(ctx context.Context, id hash.ID) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("fs store: %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("fs store: reading %s: %w", id, err)
	}
	return b, nil
}

// Put implements ObjectStore.
func (s *FS) Put(ctx context.Context, id hash.ID, stored []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dst := s.path(id)
	if _, err := os.Stat(dst); err == nil {
		return nil // already present; objects are immutable
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("fs store: creating %s: %w", dir, err)
	}
	if err := writeAtomic(dir, dst, stored); err != nil {
		return fmt.Errorf("fs store: writing %s: %w", id, err)
	}
	return nil
}

// writeAtomic writes data to a temp file in dir, fsyncs it and renames it to
// dst. Concurrent writers of the same dst are safe: each renames a complete
// file with identical content.
func writeAtomic(dir, dst string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// Has implements ObjectStore.
func (s *FS) Has(ctx context.Context, ids []hash.ID) ([]bool, error) {
	present := make([]bool, len(ids))
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, err := os.Stat(s.path(id))
		switch {
		case err == nil:
			present[i] = true
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("fs store: stat %s: %w", id, err)
		}
	}
	return present, nil
}

// List implements Lister by walking <root>/objects.
func (s *FS) List(ctx context.Context, fn func(ObjectInfo) error) error {
	dir := filepath.Join(s.root, "objects")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == dir {
				return filepath.SkipDir // nothing stored yet
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		id, ok := idFromKey(filepath.ToSlash(rel))
		if !ok {
			return nil // a temp file or something else
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return fn(ObjectInfo{ID: id, Size: info.Size(), ModTime: info.ModTime()})
	})
	if err != nil {
		return fmt.Errorf("fs store: listing: %w", err)
	}
	return nil
}
