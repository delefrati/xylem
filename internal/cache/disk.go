package cache

import (
	"container/list"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/store"
)

// Disk is an LRU of stored object frames on disk, in the fs store's key
// layout (dir/objects/ab/cd/<hex>), capped at a total size. The index is kept
// in memory and rebuilt at open from the files, oldest modification first.
// Writes are atomic (temp file + rename) but not fsynced: a cache can lose
// entries. It is safe for concurrent use.
type Disk struct {
	dir      string
	maxBytes int64

	mu    sync.Mutex
	bytes int64
	order *list.List // front = most recently used
	items map[hash.ID]*list.Element
}

type diskEntry struct {
	id   hash.ID
	size int64
}

const tmpPrefix = ".tmp-"

// OpenDisk opens (creating if needed) a disk cache in dir holding up to
// maxBytes, indexing the objects already there and evicting down to the cap.
func OpenDisk(dir string, maxBytes int64) (*Disk, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("disk cache %s: size must be positive", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	d := &Disk{dir: dir, maxBytes: maxBytes, order: list.New(), items: map[hash.ID]*list.Element{}}
	type found struct {
		id    hash.ID
		size  int64
		mtime time.Time
	}
	var all []found
	rt, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rt.Close() }()
	root := filepath.Join(dir, "objects")
	err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return err
		case e.IsDir():
			return nil
		case strings.HasPrefix(e.Name(), tmpPrefix): // left by a crash
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			return rt.Remove(rel)
		}
		id, err := hash.ParseHex(e.Name())
		if err != nil {
			return nil // not ours
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		all = append(all, found{id, info.Size(), info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("indexing disk cache %s: %w", dir, err)
	}
	slices.SortFunc(all, func(a, b found) int { return a.mtime.Compare(b.mtime) })
	for _, f := range all { // oldest first, so the newest end at the front
		d.items[f.id] = d.order.PushFront(&diskEntry{f.id, f.size})
		d.bytes += f.size
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evictLocked()
	return d, nil
}

func (d *Disk) path(id hash.ID) string {
	return filepath.Join(d.dir, filepath.FromSlash(store.Key(id)))
}

// Get returns the frame of id if cached.
func (d *Disk) Get(id hash.ID) ([]byte, bool) {
	d.mu.Lock()
	e, ok := d.items[id]
	if ok {
		d.order.MoveToFront(e)
	}
	d.mu.Unlock()
	if !ok {
		return nil, false
	}
	data, err := os.ReadFile(d.path(id))
	if err != nil { // removed behind our back: forget it
		d.remove(id)
		return nil, false
	}
	return data, true
}

// Has reports whether id is indexed.
func (d *Disk) Has(id hash.ID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.items[id]
	return ok
}

// Put stores a frame and evicts the least recently used ones over the cap.
// Frames larger than a quarter of the cap are not cached.
func (d *Disk) Put(id hash.ID, data []byte) error {
	size := int64(len(data))
	if size > d.maxBytes/4 || d.Has(id) {
		return nil
	}
	p := d.path(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), tmpPrefix+"*")
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), p); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.items[id]; !ok {
		d.items[id] = d.order.PushFront(&diskEntry{id, size})
		d.bytes += size
	}
	d.evictLocked()
	return nil
}

func (d *Disk) remove(id hash.ID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.items[id]; ok {
		d.order.Remove(e)
		delete(d.items, id)
		d.bytes -= e.Value.(*diskEntry).size
	}
}

func (d *Disk) evictLocked() {
	for d.bytes > d.maxBytes {
		oldest := d.order.Back()
		old := oldest.Value.(*diskEntry)
		d.order.Remove(oldest)
		delete(d.items, old.id)
		d.bytes -= old.size
		_ = os.Remove(d.path(old.id))
	}
}

// Stats returns the number of cached frames and their total size.
func (d *Disk) Stats() (objects int, bytes int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.items), d.bytes
}
