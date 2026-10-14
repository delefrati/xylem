package client

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
)

// dirstateMagic starts every dirstate file (06 §Workspace layout).
const dirstateMagic = "XYLDS1"

// ErrNoDirstate reports a missing or unreadable dirstate; callers rebuild it.
var ErrNoDirstate = errors.New("no usable dirstate")

// DirEntry is the recorded state of one tracked file. MtimeNs 0 forces the
// next status to hash the file.
type DirEntry struct {
	Path    []byte
	Mode    object.Mode
	Size    int64
	MtimeNs int64
	Blob    hash.ID
}

// Dirstate maps tracked paths to their last known stat and blob.
type Dirstate struct {
	WrittenAtNs int64 // when it was saved: mtimes at or after this are "racy"
	entries     map[string]DirEntry
}

// NewDirstate returns an empty dirstate.
func NewDirstate() *Dirstate { return &Dirstate{entries: map[string]DirEntry{}} }

// Get returns the entry for path.
func (d *Dirstate) Get(path []byte) (DirEntry, bool) {
	e, ok := d.entries[string(path)]
	return e, ok
}

// Set records an entry.
func (d *Dirstate) Set(e DirEntry) { d.entries[string(e.Path)] = e }

// Delete forgets path.
func (d *Dirstate) Delete(path []byte) { delete(d.entries, string(path)) }

// Len returns the number of tracked files.
func (d *Dirstate) Len() int { return len(d.entries) }

// Entries returns the entries sorted by path.
func (d *Dirstate) Entries() []DirEntry {
	out := make([]DirEntry, 0, len(d.entries))
	for _, e := range d.entries {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b DirEntry) int { return bytes.Compare(a.Path, b.Path) })
	return out
}

type dirstateFile struct {
	WrittenAtNs int64
	Entries     []DirEntry
}

// LoadDirstate reads a dirstate; a missing file or one with a bad header is
// ErrNoDirstate.
func LoadDirstate(path string) (*Dirstate, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoDirstate
	}
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(b, []byte(dirstateMagic)) {
		return nil, fmt.Errorf("%w: bad header", ErrNoDirstate)
	}
	var f dirstateFile
	if err := gob.NewDecoder(bytes.NewReader(b[len(dirstateMagic):])).Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoDirstate, err)
	}
	d := &Dirstate{WrittenAtNs: f.WrittenAtNs, entries: make(map[string]DirEntry, len(f.Entries))}
	for _, e := range f.Entries {
		d.entries[string(e.Path)] = e
	}
	return d, nil
}

// Save writes the dirstate atomically, stamping WrittenAtNs with the time.
func (d *Dirstate) Save(path string) error {
	d.WrittenAtNs = time.Now().UnixNano()
	var buf bytes.Buffer
	buf.WriteString(dirstateMagic)
	if err := gob.NewEncoder(&buf).Encode(dirstateFile{WrittenAtNs: d.WrittenAtNs, Entries: d.Entries()}); err != nil {
		return err
	}
	return WriteFileAtomic(path, buf.Bytes(), 0o644)
}

// FileMode returns the Xylem mode of a file on disk (git's rule: a regular
// file with the owner execute bit is EXEC).
func FileMode(info fs.FileInfo) (object.Mode, bool) {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return object.ModeSymlink, true
	case info.Mode().IsRegular() && info.Mode().Perm()&0o100 != 0:
		return object.ModeExec, true
	case info.Mode().IsRegular():
		return object.ModeFile, true
	}
	return 0, false
}

// HashFile returns the blob id of a file on disk (a symlink hashes its target).
func HashFile(abs string, mode object.Mode) (hash.ID, int64, error) {
	var content []byte
	var err error
	if mode == object.ModeSymlink {
		var target string
		target, err = os.Readlink(abs)
		content = []byte(target)
	} else {
		content, err = os.ReadFile(abs)
	}
	if err != nil {
		return hash.ID{}, 0, err
	}
	return object.ID(object.KindBlob, content), int64(len(content)), nil
}

// entryFor builds a dirstate entry from a file's current stat.
func entryFor(path []byte, info fs.FileInfo, mode object.Mode, blob hash.ID) DirEntry {
	return DirEntry{Path: path, Mode: mode, Size: info.Size(), MtimeNs: info.ModTime().UnixNano(), Blob: blob}
}

// RebuildDirstate hashes the working files expected by files (the checked
// out tree in scope). Files that match get their real stat; files that
// differ or are missing get MtimeNs 0, so status reports them.
func RebuildDirstate(root string, files []FileEntry) (*Dirstate, error) {
	d := NewDirstate()
	for _, f := range files {
		abs := filepath.Join(root, string(f.Path))
		e := DirEntry{Path: f.Path, Mode: f.Mode, Size: sizeInt64(f.Size), Blob: f.ID}
		if info, err := os.Lstat(abs); err == nil {
			if mode, ok := FileMode(info); ok && mode == f.Mode {
				if id, _, err := HashFile(abs, mode); err == nil && id == f.ID {
					e = entryFor(f.Path, info, mode, f.ID)
				}
			}
		}
		d.Set(e)
	}
	return d, nil
}

// sizeInt64 converts a blob size, clamping (sizes are far below 2^63).
func sizeInt64(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}
