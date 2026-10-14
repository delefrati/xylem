// Package treeops implements tree lookup and the incremental tree rewrite that
// turns a parent's root tree and a change list into a new root tree
// (04-data-model §Root tree). Cost is O(changed paths × depth): only trees on
// the changed paths are read and rewritten; untouched subtrees keep their ids.
package treeops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
	"github.com/delefrati/xylem/internal/store"
)

// EmptyTree is the id of the empty tree, the root of an empty repository.
var EmptyTree = object.ID(object.KindTree, []byte{0})

var (
	// ErrDeleteMissing reports a delete of a path that is not a file in the parent tree.
	ErrDeleteMissing = errors.New("delete of a path that does not exist")
	// ErrNoopPut reports a put identical (same mode and blob) to the parent's entry.
	ErrNoopPut = errors.New("no-op change")
	// ErrNotTree reports an object that is not a tree where a tree was expected.
	ErrNotTree = errors.New("object is not a tree")
	// ErrDuplicatePath reports two changes to the same path.
	ErrDuplicatePath = errors.New("duplicate change for path")
)

// FileDirClashError reports a path that would be both a file and a directory.
type FileDirClashError struct {
	Path []byte
}

func (e *FileDirClashError) Error() string {
	return "path " + strconv.Quote(string(e.Path)) + " would be both a file and a directory"
}

// TreeReader reads decoded trees by id. The returned trees are shared and
// must not be modified.
type TreeReader interface {
	ReadTree(ctx context.Context, id hash.ID) (*object.Tree, error)
}

// Trees reads trees from an object store through an LRU of decoded trees.
// It is safe for concurrent use.
type Trees struct {
	objs  store.ObjectStore
	cache *lru.Cache[hash.ID, *object.Tree]
}

// NewTrees returns a Trees caching up to cacheSize decoded trees.
func NewTrees(objs store.ObjectStore, cacheSize int) *Trees {
	cache, err := lru.New[hash.ID, *object.Tree](max(cacheSize, 1))
	if err != nil {
		panic(err) // only for a non-positive size
	}
	return &Trees{objs: objs, cache: cache}
}

var emptyTree = &object.Tree{}

// ReadTree implements TreeReader. EmptyTree is served without a store read.
func (t *Trees) ReadTree(ctx context.Context, id hash.ID) (*object.Tree, error) {
	if id == EmptyTree {
		return emptyTree, nil
	}
	if tree, ok := t.cache.Get(id); ok {
		return tree, nil
	}
	stored, err := t.objs.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading tree %s: %w", id, err)
	}
	kind, body, err := object.Decode(stored)
	if err != nil {
		return nil, fmt.Errorf("reading tree %s: %w", id, err)
	}
	if kind != object.KindTree {
		return nil, fmt.Errorf("reading tree %s: %w (it is a %v)", id, ErrNotTree, kind)
	}
	tree, err := object.DecodeTree(body)
	if err != nil {
		return nil, fmt.Errorf("reading tree %s: %w", id, err)
	}
	t.cache.Add(id, tree)
	return tree, nil
}

// Add caches a decoded tree, e.g. one just produced by Apply.
func (t *Trees) Add(id hash.ID, tree *object.Tree) {
	t.cache.Add(id, tree)
}

// Lookup returns the entry at path under root. The empty path is the root
// itself, returned as a ModeTree entry.
func Lookup(ctx context.Context, r TreeReader, root hash.ID, path []byte) (object.Entry, bool, error) {
	if len(path) == 0 {
		return object.Entry{Mode: object.ModeTree, ID: root}, true, nil
	}
	cur := root
	names := paths.Split(path)
	for i, name := range names {
		tree, err := r.ReadTree(ctx, cur)
		if err != nil {
			return object.Entry{}, false, err
		}
		e, ok := tree.Lookup(name)
		if !ok {
			return object.Entry{}, false, nil
		}
		if i == len(names)-1 {
			return e, true, nil
		}
		if e.Mode != object.ModeTree {
			return object.Entry{}, false, nil
		}
		cur = e.ID
	}
	panic("unreachable")
}

// NewTree is a tree written by Apply: its id, stored form and decoded form.
type NewTree struct {
	ID     hash.ID
	Stored []byte
	Tree   *object.Tree
}

// Apply applies changes (any order) to the tree root and returns the new root
// and the trees it created, bottom-up. Deletes apply first, pruning
// directories they leave empty (except the root); then puts. It validates 03
// §Validation rule 4: deleting a missing path (ErrDeleteMissing), a put equal
// to the existing entry (ErrNoopPut), and a path that would be both a file and
// a directory (*FileDirClashError); and rejects two changes to one path
// (ErrDuplicatePath). The zero root means the empty tree.
func Apply(ctx context.Context, r TreeReader, root hash.ID, changes []changeset.Change) (hash.ID, []NewTree, error) {
	if root.IsZero() {
		root = EmptyTree
	}
	if len(changes) == 0 {
		return root, nil, nil
	}
	sorted := slices.Clone(changes)
	slices.SortFunc(sorted, func(a, b changeset.Change) int { return bytes.Compare(a.Path, b.Path) })
	for i := 1; i < len(sorted); i++ {
		if bytes.Equal(sorted[i-1].Path, sorted[i].Path) {
			return hash.ID{}, nil, fmt.Errorf("%w %q", ErrDuplicatePath, sorted[i].Path)
		}
	}

	top := &dir{id: root}
	for _, ch := range sorted {
		if ch.Op == changeset.OpDelete {
			if err := top.delete(ctx, r, ch.Path, ch.Path); err != nil {
				return hash.ID{}, nil, err
			}
		}
	}
	for _, ch := range sorted {
		switch ch.Op {
		case changeset.OpDelete:
		case changeset.OpPut:
			if ch.Mode == object.ModeTree || !ch.Mode.Valid() {
				return hash.ID{}, nil, fmt.Errorf("put %q: invalid mode %v", ch.Path, ch.Mode)
			}
			if err := top.put(ctx, r, ch); err != nil {
				return hash.ID{}, nil, err
			}
		default:
			return hash.ID{}, nil, fmt.Errorf("change %q: invalid op %v", ch.Path, ch.Op)
		}
	}
	var created []NewTree
	newRoot, err := top.encode(&created)
	if err != nil {
		return hash.ID{}, nil, err
	}
	return newRoot, created, nil
}

// dir is a mutable directory during Apply. Its entries are loaded on first
// modification; subdirectories that are modified get their own dir.
type dir struct {
	id      hash.ID // original tree id; zero for a new directory
	loaded  bool
	entries []object.Entry // sorted by name; copied from the original tree
	subdirs map[string]*dir
	dirty   bool
}

func (d *dir) load(ctx context.Context, r TreeReader) error {
	if d.loaded {
		return nil
	}
	if !d.id.IsZero() {
		tree, err := r.ReadTree(ctx, d.id)
		if err != nil {
			return err
		}
		d.entries = slices.Clone(tree.Entries)
	}
	d.loaded = true
	d.subdirs = make(map[string]*dir)
	return nil
}

func (d *dir) find(name []byte) (int, bool) {
	return slices.BinarySearchFunc(d.entries, name, func(e object.Entry, name []byte) int {
		return bytes.Compare(e.Name, name)
	})
}

// child returns the dir for subdirectory entry i, creating it on first use.
func (d *dir) child(i int) *dir {
	name := string(d.entries[i].Name)
	sub, ok := d.subdirs[name]
	if !ok {
		sub = &dir{id: d.entries[i].ID}
		d.subdirs[name] = sub
	}
	return sub
}

// delete removes the file at path (full is the whole path, for errors) and
// prunes directories it leaves empty.
func (d *dir) delete(ctx context.Context, r TreeReader, path, full []byte) error {
	name, rest, nested := bytes.Cut(path, []byte{'/'})
	if err := d.load(ctx, r); err != nil {
		return err
	}
	i, ok := d.find(name)
	if !ok {
		return fmt.Errorf("%w: %q", ErrDeleteMissing, full)
	}
	e := d.entries[i]
	if !nested {
		if e.Mode == object.ModeTree {
			return fmt.Errorf("%w: %q is a directory", ErrDeleteMissing, full)
		}
		d.entries = slices.Delete(d.entries, i, i+1)
		d.dirty = true
		return nil
	}
	if e.Mode != object.ModeTree {
		return fmt.Errorf("%w: %q (%q is a file)", ErrDeleteMissing, full, full[:len(full)-len(rest)-1])
	}
	sub := d.child(i)
	if err := sub.delete(ctx, r, rest, full); err != nil {
		return err
	}
	d.dirty = true
	if len(sub.entries) == 0 {
		d.entries = slices.Delete(d.entries, i, i+1)
		delete(d.subdirs, string(name))
	}
	return nil
}

// put creates or replaces the file at ch.Path, creating directories as needed.
func (d *dir) put(ctx context.Context, r TreeReader, ch changeset.Change) error {
	return d.putAt(ctx, r, ch.Path, ch, ch.Path)
}

func (d *dir) putAt(ctx context.Context, r TreeReader, path []byte, ch changeset.Change, full []byte) error {
	name, rest, nested := bytes.Cut(path, []byte{'/'})
	if err := d.load(ctx, r); err != nil {
		return err
	}
	i, ok := d.find(name)
	d.dirty = true
	if !nested {
		if ok && d.entries[i].Mode == object.ModeTree {
			return &FileDirClashError{Path: bytes.Clone(full)}
		}
		e := object.Entry{Mode: ch.Mode, Name: bytes.Clone(name), ID: ch.Blob, Size: ch.Size}
		if ok {
			if old := d.entries[i]; old.Mode == ch.Mode && old.ID == ch.Blob {
				return fmt.Errorf("%w: %q", ErrNoopPut, full)
			}
			d.entries[i] = e
		} else {
			d.entries = slices.Insert(d.entries, i, e)
		}
		return nil
	}
	if !ok {
		d.entries = slices.Insert(d.entries, i, object.Entry{Mode: object.ModeTree, Name: bytes.Clone(name)})
		d.subdirs[string(name)] = &dir{loaded: true, subdirs: map[string]*dir{}}
	} else if d.entries[i].Mode != object.ModeTree {
		return &FileDirClashError{Path: bytes.Clone(full[:len(full)-len(rest)-1])}
	}
	return d.child(i).putAt(ctx, r, rest, ch, full)
}

// encode writes dirty subdirectories bottom-up and returns d's id.
func (d *dir) encode(created *[]NewTree) (hash.ID, error) {
	if !d.dirty {
		return d.id, nil
	}
	for i, e := range d.entries {
		sub, ok := d.subdirs[string(e.Name)]
		if !ok || e.Mode != object.ModeTree {
			continue
		}
		id, err := sub.encode(created)
		if err != nil {
			return hash.ID{}, err
		}
		d.entries[i].ID = id
	}
	tree := &object.Tree{Entries: d.entries}
	body, err := object.EncodeTree(tree)
	if err != nil {
		return hash.ID{}, err
	}
	id, stored := object.Encode(object.KindTree, body)
	*created = append(*created, NewTree{ID: id, Stored: stored, Tree: tree})
	return id, nil
}
