package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
	"github.com/delefrati/xylem/internal/treeops"
)

// Change is one file a checkout touches: Old is what the current tree has
// (nil if absent or out of scope), New what the target has.
type Change struct {
	Path []byte
	Old  *FileEntry
	New  *FileEntry
}

// CheckoutResult summarizes a checkout.
type CheckoutResult struct {
	Written, Deleted int
	Fetched          FetchStats
}

// DirtyError lists working files a checkout would overwrite.
type DirtyError struct{ Paths [][]byte }

func (e *DirtyError) Error() string {
	var names []string
	for _, p := range e.Paths[:min(len(e.Paths), 10)] {
		names = append(names, string(p))
	}
	more := ""
	if len(e.Paths) > 10 {
		more = fmt.Sprintf(" and %d more", len(e.Paths)-10)
	}
	return fmt.Sprintf("local changes would be overwritten: %s%s (use --force to discard them)", strings.Join(names, ", "), more)
}

// Target is where a checkout moves the working copy.
type Target struct {
	Changeset hash.ID
	Root      hash.ID
	Sparse    []Path // the sparse profile to apply (nil: keep the current one)
}

// LoadDirstateFor returns the workspace's dirstate, rebuilding it by hashing
// if it is missing, unreadable or describes another tree than the state.
func (w *Workspace) LoadDirstateFor(ctx context.Context, f *Fetcher) (*Dirstate, error) {
	ds, err := LoadDirstate(w.Meta("dirstate"))
	if err == nil && ds.Root == w.State.RootTree {
		return ds, nil
	}
	if err != nil && !errors.Is(err, ErrNoDirstate) {
		return nil, err
	}
	if w.State.RootTree.IsZero() {
		d := NewDirstate()
		return d, nil
	}
	files, err := f.Fetch(ctx, w.State.RootTree, w.Scope())
	if err != nil {
		return nil, err
	}
	return RebuildDirstate(w.Root, w.State.RootTree, files)
}

// Scope returns the workspace's sparse scope.
func (w *Workspace) Scope() Scope {
	return scopeOf(w.State.Sparse)
}

func scopeOf(sparse []Path) Scope {
	dirs := make([][]byte, len(sparse))
	for i, p := range sparse {
		dirs[i] = p
	}
	return NewScope(dirs)
}

func sameScope(a, b []Path) bool {
	return slices.EqualFunc(a, b, func(x, y Path) bool { return bytes.Equal(x, y) })
}

// Checkout moves the working copy to target (06 §Checkout): scoped tree
// diff, dirty check, fetch, apply, then dirstate and finally state.json.
// The caller holds the workspace lock.
func (w *Workspace) Checkout(ctx context.Context, f *Fetcher, target Target, force bool) (*CheckoutResult, error) {
	ds, err := w.LoadDirstateFor(ctx, f)
	if err != nil {
		return nil, err
	}
	newSparse := w.State.Sparse
	if target.Sparse != nil {
		newSparse = target.Sparse
	}
	var changes []Change
	if sameScope(newSparse, w.State.Sparse) && !w.State.RootTree.IsZero() {
		changes, err = scopedDiff(ctx, CacheTrees{F: f}, w.State.RootTree, target.Root, scopeOf(newSparse))
	} else {
		var want []FileEntry
		if want, err = f.Fetch(ctx, target.Root, scopeOf(newSparse)); err == nil {
			changes = reconcile(want, ds)
		}
	}
	if err != nil {
		return nil, err
	}

	if force { // also discard local changes to files the move leaves alone
		if changes, err = w.restoreModified(ds, changes); err != nil {
			return nil, err
		}
	} else {
		if dirty, err := w.dirty(ds, changes); err != nil {
			return nil, err
		} else if len(dirty) > 0 {
			return nil, &ExitError{Code: ExitDirty, Err: &DirtyError{Paths: dirty}}
		}
	}
	var blobs []hash.ID
	for _, c := range changes {
		if c.New != nil {
			blobs = append(blobs, c.New.ID)
		}
	}
	if err := f.Ensure(ctx, blobs); err != nil {
		return nil, err
	}

	res := &CheckoutResult{}
	for _, c := range changes { // deletes first: they make room for type changes
		if c.Old != nil && c.New == nil {
			if err := w.remove(c.Path); err != nil {
				return nil, err
			}
			ds.Delete(c.Path)
			res.Deleted++
		}
	}
	for _, c := range changes {
		if c.New == nil {
			continue
		}
		info, err := w.write(f.cache, *c.New, force)
		if err != nil {
			return nil, err
		}
		ds.Set(entryFor(c.Path, info, c.New.Mode, c.New.ID))
		res.Written++
	}
	ds.Root = target.Root
	if err := ds.Save(w.Meta("dirstate")); err != nil {
		return nil, err
	}
	w.State.Parent, w.State.RootTree, w.State.Sparse = target.Changeset, target.Root, newSparse
	if err := w.SaveState(); err != nil { // last: a crash before this keeps the old state
		return nil, err
	}
	res.Fetched = f.Stats()
	return res, f.cache.EnsureLimit()
}

// dirty returns the paths a checkout would clobber: tracked files with local
// changes, untracked files where a new file goes, and anything but clean
// to-be-deleted tracked files inside a directory that a new file replaces.
func (w *Workspace) dirty(ds *Dirstate, changes []Change) ([][]byte, error) {
	deleted := map[string]bool{}
	for _, c := range changes {
		if c.Old != nil && c.New == nil {
			deleted[string(c.Path)] = true
		}
	}
	var out [][]byte
	for _, c := range changes {
		_, tracked := ds.Get(c.Path)
		fc, err := checkFile(w.Root, ds, c.Path)
		if err != nil {
			return nil, err
		}
		switch {
		case !fc.exists:
		case fc.info.IsDir():
			if c.New != nil { // a directory in the way of a new file
				lost, err := w.dirtyInside(ds, c.Path, deleted)
				if err != nil {
					return nil, err
				}
				out = append(out, lost...)
			}
		case tracked && !fc.clean:
			out = append(out, c.Path) // modified
		case !tracked && c.New != nil && (fc.mode != c.New.Mode || fc.blob != c.New.ID):
			out = append(out, c.Path) // untracked file in the way
		}
	}
	return out, nil
}

// restoreModified adds a rewrite for every tracked file outside changes that
// differs from the dirstate, which matches the target there (--force).
func (w *Workspace) restoreModified(ds *Dirstate, changes []Change) ([]Change, error) {
	inChanges := make(map[string]bool, len(changes))
	for _, c := range changes {
		inChanges[string(c.Path)] = true
	}
	for _, e := range ds.Entries() {
		if inChanges[string(e.Path)] {
			continue
		}
		fc, err := checkFile(w.Root, ds, e.Path)
		if err != nil {
			return nil, err
		}
		if !fc.clean {
			f := &FileEntry{Path: e.Path, Mode: e.Mode, ID: e.Blob}
			changes = append(changes, Change{Path: e.Path, Old: f, New: f})
		}
	}
	return changes, nil
}

// dirtyInside lists files under dir that removing it would lose: anything
// that is not a clean tracked file this checkout deletes anyway.
func (w *Workspace) dirtyInside(ds *Dirstate, dir []byte, deleted map[string]bool) ([][]byte, error) {
	var out [][]byte
	base := filepath.Join(w.Root, string(dir))
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(w.Root, p)
		if err != nil {
			return err
		}
		path := []byte(filepath.ToSlash(rel))
		if !deleted[string(path)] {
			out = append(out, path)
			return nil
		}
		fc, err := checkFile(w.Root, ds, path)
		if err != nil {
			return err
		}
		if !fc.clean {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// scopedDiff compares two trees file by file inside scope, skipping
// identical subtrees and never reading out-of-scope trees.
func scopedDiff(ctx context.Context, r treeops.TreeReader, cur, tgt hash.ID, scope Scope) ([]Change, error) {
	var out []Change
	var walk func(a, b hash.ID, prefix []byte) error
	walk = func(a, b hash.ID, prefix []byte) error {
		if a == b {
			return nil
		}
		ta, tb := &object.Tree{}, &object.Tree{}
		var err error
		if !a.IsZero() {
			if ta, err = r.ReadTree(ctx, a); err != nil {
				return err
			}
		}
		if !b.IsZero() {
			if tb, err = r.ReadTree(ctx, b); err != nil {
				return err
			}
		}
		names := map[string][2]*object.Entry{}
		for i := range ta.Entries {
			e := &ta.Entries[i]
			v := names[string(e.Name)]
			v[0] = e
			names[string(e.Name)] = v
		}
		for i := range tb.Entries {
			e := &tb.Entries[i]
			v := names[string(e.Name)]
			v[1] = e
			names[string(e.Name)] = v
		}
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for n := range names {
				if !yield(n) {
					return
				}
			}
		}) {
			ea, eb := names[name][0], names[name][1]
			p := []byte(name)
			if len(prefix) > 0 {
				p = paths.Join(prefix, []byte(name))
			}
			var oldF, newF *FileEntry
			var subA, subB hash.ID
			if ea != nil {
				if ea.Mode == object.ModeTree {
					subA = ea.ID
				} else if scope.FileInScope(p) {
					oldF = &FileEntry{Path: p, Mode: ea.Mode, ID: ea.ID, Size: ea.Size}
				}
			}
			if eb != nil {
				if eb.Mode == object.ModeTree {
					subB = eb.ID
				} else if scope.FileInScope(p) {
					newF = &FileEntry{Path: p, Mode: eb.Mode, ID: eb.ID, Size: eb.Size}
				}
			}
			if oldF != nil || newF != nil {
				if oldF == nil || newF == nil || oldF.ID != newF.ID || oldF.Mode != newF.Mode {
					out = append(out, Change{Path: p, Old: oldF, New: newF})
				}
			}
			if (!subA.IsZero() || !subB.IsZero()) && scope.TreeInScope(p) {
				if err := walk(subA, subB, p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(cur, tgt, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// reconcile compares the wanted files with the dirstate (used when the sparse
// profile changes, or from an empty working copy).
func reconcile(want []FileEntry, ds *Dirstate) []Change {
	var out []Change
	wanted := map[string]bool{}
	for i := range want {
		f := &want[i]
		wanted[string(f.Path)] = true
		e, ok := ds.Get(f.Path)
		if ok && e.Mode == f.Mode && e.Blob == f.ID && e.MtimeNs != 0 {
			continue
		}
		c := Change{Path: f.Path, New: f}
		if ok {
			c.Old = &FileEntry{Path: e.Path, Mode: e.Mode, ID: e.Blob}
		}
		out = append(out, c)
	}
	for _, e := range ds.Entries() {
		if !wanted[string(e.Path)] {
			out = append(out, Change{Path: e.Path, Old: &FileEntry{Path: e.Path, Mode: e.Mode, ID: e.Blob}})
		}
	}
	return out
}

// remove deletes a working file and any directories it leaves empty.
func (w *Workspace) remove(path []byte) error {
	abs := filepath.Join(w.Root, string(path))
	if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for dir := filepath.Dir(abs); dir != w.Root && len(dir) > len(w.Root); dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			break // not empty (or already gone)
		}
	}
	return nil
}

// write materializes a file atomically (temp + fsync + rename), creating
// parent directories and removing whatever is in the way (type changes).
func (w *Workspace) write(cache *ObjectCache, f FileEntry, force bool) (fs.FileInfo, error) {
	abs := filepath.Join(w.Root, string(f.Path))
	if err := w.makeParents(f.Path); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(abs); err == nil && info.IsDir() {
		if !force {
			if entries, _ := os.ReadDir(abs); len(entries) > 0 {
				return nil, &ExitError{Code: ExitDirty, Err: fmt.Errorf("%q is a non-empty directory in the way of a file (use --force)", f.Path)}
			}
		}
		if err := os.RemoveAll(abs); err != nil {
			return nil, err
		}
	}
	kind, content, err := cache.GetVerified(f.ID)
	if err != nil {
		return nil, err
	}
	if kind != object.KindBlob {
		return nil, fmt.Errorf("%s is a %v, not a blob", f.ID.Short(12), kind)
	}
	if f.Mode == object.ModeSymlink {
		tmp := filepath.Join(filepath.Dir(abs), ".xy-link-"+filepath.Base(abs))
		_ = os.Remove(tmp)
		if err := os.Symlink(string(content), tmp); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, abs); err != nil {
			return nil, err
		}
	} else {
		perm := fs.FileMode(0o644)
		if f.Mode == object.ModeExec {
			perm = 0o755
		}
		if err := WriteFileAtomic(abs, content, perm); err != nil {
			return nil, err
		}
	}
	return os.Lstat(abs)
}

// makeParents creates the directories of path, replacing files or symlinks
// that are in the way (a file that became a directory).
func (w *Workspace) makeParents(path []byte) error {
	dir := w.Root
	parts := paths.Split(paths.Parent(path))
	for _, part := range parts {
		dir = filepath.Join(dir, string(part))
		info, err := os.Lstat(dir)
		switch {
		case err == nil && info.IsDir():
			continue
		case err == nil:
			if err := os.Remove(dir); err != nil {
				return err
			}
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // G301: working tree directories are world-readable, like git's
			return err
		}
	}
	return nil
}
