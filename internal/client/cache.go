package client

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/store"
)

// ObjectCache is the workspace's local object cache: stored frames under
// .xylem/objects/ab/cd/<hex>, verified on insert, evicted least recently
// used first. A file's mtime is its last-access time (set on every read), so
// eviction does not depend on atime support (relatime/noatime mounts).
type ObjectCache struct {
	dir      string
	maxBytes int64
}

// ErrCacheMiss reports an object that is not in the cache.
var ErrCacheMiss = errors.New("object not in local cache")

// NewObjectCache returns the cache in dir with the given size limit (≤ 0: unlimited).
// A negative maxBytes keeps objects only until EnsureLimit (no persistent
// cache, for workspaces behind a daemon); 0 means unbounded.
func NewObjectCache(dir string, maxBytes int64) *ObjectCache {
	return &ObjectCache{dir: dir, maxBytes: maxBytes}
}

func (c *ObjectCache) path(id hash.ID) string {
	// store.Key is "objects/ab/cd/<hex>"; the cache directory already is "objects".
	return filepath.Join(c.dir, filepath.FromSlash(store.Key(id)[len("objects/"):]))
}

// Has reports whether id is cached.
func (c *ObjectCache) Has(id hash.ID) bool {
	_, err := os.Stat(c.path(id))
	return err == nil
}

// Put verifies stored against id and caches it.
func (c *ObjectCache) Put(id hash.ID, stored []byte) error {
	if err := object.Verify(id, stored); err != nil {
		return fmt.Errorf("refusing to cache %s: %w", id.Short(12), err)
	}
	p := c.path(id)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	return WriteFileAtomic(p, stored, 0o644)
}

// Get returns the stored frame of id and marks it recently used.
func (c *ObjectCache) Get(id hash.ID) ([]byte, error) {
	p := c.path(id)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", id.Short(12), ErrCacheMiss)
	}
	if err != nil {
		return nil, err
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now) // access clock for LRU eviction
	return b, nil
}

// GetVerified returns the decoded object, checking it against id (a corrupt
// cache file is removed and reported as a miss).
func (c *ObjectCache) GetVerified(id hash.ID) (object.Kind, []byte, error) {
	stored, err := c.Get(id)
	if err != nil {
		return 0, nil, err
	}
	kind, body, err := object.DecodeVerify(id, stored)
	if err != nil {
		_ = os.Remove(c.path(id))
		return 0, nil, fmt.Errorf("%s: corrupt cache entry removed: %w", id.Short(12), ErrCacheMiss)
	}
	return kind, body, nil
}

// CacheStats describes the cache.
type CacheStats struct {
	Objects  int   `json:"objects"`
	Bytes    int64 `json:"bytes"`
	MaxBytes int64 `json:"max_bytes"`
}

type cachedFile struct {
	path   string
	size   int64
	usedAt time.Time
}

func (c *ObjectCache) scan() ([]cachedFile, error) {
	var files []cachedFile
	err := filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || filepath.Base(p)[0] == '.' { // skip temp files
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // removed concurrently
		}
		files = append(files, cachedFile{p, info.Size(), info.ModTime()})
		return nil
	})
	return files, err
}

// Stats counts the cached objects and bytes.
func (c *ObjectCache) Stats() (CacheStats, error) {
	files, err := c.scan()
	if err != nil {
		return CacheStats{}, err
	}
	s := CacheStats{Objects: len(files), MaxBytes: c.maxBytes}
	for _, f := range files {
		s.Bytes += f.size
	}
	return s, nil
}

// GC evicts least recently used objects until the cache holds at most
// target bytes, and returns how many objects and bytes it removed.
func (c *ObjectCache) GC(target int64) (removed int, freed int64, err error) {
	files, err := c.scan()
	if err != nil {
		return 0, 0, err
	}
	var total int64
	for _, f := range files {
		total += f.size
	}
	slices.SortFunc(files, func(a, b cachedFile) int { return a.usedAt.Compare(b.usedAt) })
	for _, f := range files {
		if total <= target {
			break
		}
		if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, freed, err
		}
		total -= f.size
		removed++
		freed += f.size
	}
	return removed, freed, nil
}

// EnsureLimit evicts down to 90% of the limit if the cache is over it.
func (c *ObjectCache) EnsureLimit() error {
	switch {
	case c.maxBytes < 0: // no persistent cache: drop what this command fetched
		_, _, err := c.GC(0)
		return err
	case c.maxBytes == 0: // unbounded
		return nil
	}
	s, err := c.Stats()
	if err != nil || s.Bytes <= c.maxBytes {
		return err
	}
	_, _, err = c.GC(c.maxBytes / 10 * 9)
	return err
}
