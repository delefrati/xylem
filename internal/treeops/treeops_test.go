package treeops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/store"
)

type file struct {
	mode object.Mode
	blob hash.ID
	size uint64
}

func blob(content string) file {
	return file{mode: object.ModeFile, blob: object.ID(object.KindBlob, []byte(content)), size: uint64(len(content))}
}

// buildFromScratch builds the tree for a flat path → file map, independently
// of Apply, and returns its root id. Trees are written to objs.
func buildFromScratch(t *testing.T, objs store.ObjectStore, files map[string]file) hash.ID {
	t.Helper()
	type node struct {
		files map[string]file
		dirs  map[string]*node
	}
	newNode := func() *node { return &node{files: map[string]file{}, dirs: map[string]*node{}} }
	root := newNode()
	for p, f := range files {
		n := root
		parts := strings.Split(p, "/")
		for _, d := range parts[:len(parts)-1] {
			if n.dirs[d] == nil {
				n.dirs[d] = newNode()
			}
			n = n.dirs[d]
		}
		n.files[parts[len(parts)-1]] = f
	}
	var write func(n *node) hash.ID
	write = func(n *node) hash.ID {
		var entries []object.Entry
		for name, f := range n.files {
			entries = append(entries, object.Entry{Mode: f.mode, Name: []byte(name), ID: f.blob, Size: f.size})
		}
		for name, sub := range n.dirs {
			entries = append(entries, object.Entry{Mode: object.ModeTree, Name: []byte(name), ID: write(sub)})
		}
		slices.SortFunc(entries, func(a, b object.Entry) int { return bytes.Compare(a.Name, b.Name) })
		body, err := object.EncodeTree(&object.Tree{Entries: entries})
		if err != nil {
			t.Fatal(err)
		}
		id, stored := object.Encode(object.KindTree, body)
		if err := objs.Put(context.Background(), id, stored); err != nil {
			t.Fatal(err)
		}
		return id
	}
	return write(root)
}

func put(path string, f file) changeset.Change {
	return changeset.Change{Path: []byte(path), Op: changeset.OpPut, Mode: f.mode, Size: f.size, Blob: f.blob}
}

func del(path string) changeset.Change {
	return changeset.Change{Path: []byte(path), Op: changeset.OpDelete}
}

// apply runs Apply and stores the created trees.
func apply(t *testing.T, trees *Trees, objs store.ObjectStore, root hash.ID, changes ...changeset.Change) (hash.ID, []NewTree, error) {
	t.Helper()
	newRoot, created, err := Apply(context.Background(), trees, root, changes)
	for _, nt := range created {
		if err := objs.Put(context.Background(), nt.ID, nt.Stored); err != nil {
			t.Fatal(err)
		}
		trees.Add(nt.ID, nt.Tree)
	}
	return newRoot, created, err
}

var base = map[string]file{
	"README":       blob("readme"),
	"a/x":          blob("ax"),
	"a/y":          blob("ay"),
	"b/c/d/deep":   blob("deep"),
	"b/other":      blob("other"),
	"services/svc": blob("svc"),
}

func TestApplyValid(t *testing.T) {
	t.Parallel()
	exec := blob("readme")
	exec.mode = object.ModeExec
	tests := []struct {
		name    string
		changes []changeset.Change
		want    func(m map[string]file)
	}{
		{"no changes", nil, func(map[string]file) {}},
		{"modify", []changeset.Change{put("a/x", blob("new"))}, func(m map[string]file) { m["a/x"] = blob("new") }},
		{"mode only", []changeset.Change{put("README", exec)}, func(m map[string]file) { m["README"] = exec }},
		{"add deep", []changeset.Change{put("n/e/w/f", blob("f"))}, func(m map[string]file) { m["n/e/w/f"] = blob("f") }},
		{"delete prunes empty dirs", []changeset.Change{del("b/c/d/deep")}, func(m map[string]file) { delete(m, "b/c/d/deep") }},
		{"replace dir with file", []changeset.Change{del("a/x"), del("a/y"), put("a", blob("now a file"))}, func(m map[string]file) {
			delete(m, "a/x")
			delete(m, "a/y")
			m["a"] = blob("now a file")
		}},
		{"replace file with dir", []changeset.Change{del("README"), put("README/inside", blob("i"))}, func(m map[string]file) {
			delete(m, "README")
			m["README/inside"] = blob("i")
		}},
		{"delete and re-add in a pruned dir", []changeset.Change{del("a/x"), del("a/y"), put("a/z", blob("z"))}, func(m map[string]file) {
			delete(m, "a/x")
			delete(m, "a/y")
			m["a/z"] = blob("z")
		}},
		{"prefix-like sibling names", []changeset.Change{put("serv", blob("s")), put("services-x", blob("x"))}, func(m map[string]file) {
			m["serv"] = blob("s")
			m["services-x"] = blob("x")
		}},
		{
			"delete everything",
			[]changeset.Change{del("README"), del("a/x"), del("a/y"), del("b/c/d/deep"), del("b/other"), del("services/svc")},
			func(m map[string]file) { clear(m) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objs := store.NewMem()
			trees := NewTrees(objs, 100)
			root := buildFromScratch(t, objs, base)
			got, _, err := apply(t, trees, objs, root, tt.changes...)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			want := maps.Clone(base)
			tt.want(want)
			if wantRoot := buildFromScratch(t, objs, want); got != wantRoot {
				t.Errorf("root = %s, want %s", got.Short(12), wantRoot.Short(12))
			}
		})
	}
}

func TestApplyEmptyRoot(t *testing.T) {
	t.Parallel()
	objs := store.NewMem()
	trees := NewTrees(objs, 10)
	for _, root := range []hash.ID{{}, EmptyTree} {
		got, created, err := apply(t, trees, objs, root, put("f", blob("f")))
		if err != nil || got != buildFromScratch(t, objs, map[string]file{"f": blob("f")}) || len(created) != 1 {
			t.Errorf("Apply on empty root %s = %s, %d trees, %v", root.Short(8), got.Short(8), len(created), err)
		}
		if got, _, err := Apply(context.Background(), trees, root, nil); err != nil || got != EmptyTree {
			t.Errorf("no changes on empty root = %s, %v; want EmptyTree", got.Short(8), err)
		}
	}
	if EmptyTree != buildFromScratch(t, objs, nil) {
		t.Error("EmptyTree differs from the encoded empty tree")
	}
	// Deleting the last file yields the empty root, not an error.
	root := buildFromScratch(t, objs, map[string]file{"d/f": blob("f")})
	if got, _, err := apply(t, trees, objs, root, del("d/f")); err != nil || got != EmptyTree {
		t.Errorf("delete last file = %s, %v; want EmptyTree", got.Short(8), err)
	}
}

func TestApplyErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		changes   []changeset.Change
		want      error
		clashPath string
	}{
		{"delete missing", []changeset.Change{del("nope")}, ErrDeleteMissing, ""},
		{"delete missing in dir", []changeset.Change{del("a/nope")}, ErrDeleteMissing, ""},
		{"delete directory", []changeset.Change{del("a")}, ErrDeleteMissing, ""},
		{"delete through file", []changeset.Change{del("README/x")}, ErrDeleteMissing, ""},
		{"no-op put", []changeset.Change{put("a/x", blob("ax"))}, ErrNoopPut, ""},
		{"put under file", []changeset.Change{put("README/x", blob("x"))}, nil, "README"},
		{"put deep under file", []changeset.Change{put("b/other/x/y", blob("x"))}, nil, "b/other"},
		{"put over dir", []changeset.Change{put("b/c", blob("x"))}, nil, "b/c"},
		{"put file and child", []changeset.Change{put("new", blob("1")), put("new/child", blob("2"))}, nil, "new"},
		{"replace dir but keep a file", []changeset.Change{del("a/x"), put("a", blob("f"))}, nil, "a"},
		{"duplicate path", []changeset.Change{del("a/x"), put("a/x", blob("new"))}, ErrDuplicatePath, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objs := store.NewMem()
			trees := NewTrees(objs, 100)
			root := buildFromScratch(t, objs, base)
			_, _, err := Apply(context.Background(), trees, root, tt.changes)
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("Apply error = %v, want %v", err, tt.want)
			}
			if tt.clashPath != "" {
				var clash *FileDirClashError
				if !errors.As(err, &clash) || string(clash.Path) != tt.clashPath {
					t.Fatalf("Apply error = %v, want FileDirClashError at %q", err, tt.clashPath)
				}
			}
		})
	}
	objs := store.NewMem()
	root := buildFromScratch(t, objs, base)
	bad := put("x", blob("x"))
	bad.Mode = object.ModeTree
	if _, _, err := Apply(context.Background(), NewTrees(objs, 10), root, []changeset.Change{bad}); err == nil {
		t.Error("put with ModeTree accepted")
	}
}

func TestApplyRewritesOnlyChangedPath(t *testing.T) {
	t.Parallel()
	objs := store.NewMem()
	trees := NewTrees(objs, 100)
	root := buildFromScratch(t, objs, base)
	newRoot, created, err := apply(t, trees, objs, root, put("b/c/d/deep", blob("changed")))
	if err != nil {
		t.Fatal(err)
	}
	// d, c, b and the root are rewritten; nothing else.
	if len(created) != 4 || created[len(created)-1].ID != newRoot {
		t.Errorf("created %d trees (root last = %v), want 4 bottom-up", len(created), len(created) > 0 && created[len(created)-1].ID == newRoot)
	}
	for _, p := range []string{"a", "services"} {
		before, _, _ := Lookup(context.Background(), trees, root, []byte(p))
		after, _, _ := Lookup(context.Background(), trees, newRoot, []byte(p))
		if before.ID != after.ID {
			t.Errorf("untouched subtree %s changed id", p)
		}
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()
	objs := store.NewMem()
	trees := NewTrees(objs, 100)
	root := buildFromScratch(t, objs, base)
	tests := []struct {
		path  string
		found bool
		mode  object.Mode
	}{
		{"", true, object.ModeTree},
		{"README", true, object.ModeFile},
		{"a", true, object.ModeTree},
		{"b/c/d/deep", true, object.ModeFile},
		{"b/c/d", true, object.ModeTree},
		{"nope", false, 0},
		{"a/nope", false, 0},
		{"README/x", false, 0},
		{"serv", false, 0},
	}
	for _, tt := range tests {
		e, found, err := Lookup(context.Background(), trees, root, []byte(tt.path))
		if err != nil || found != tt.found || (found && e.Mode != tt.mode) {
			t.Errorf("Lookup(%q) = %v %v, %v; want found=%v mode %v", tt.path, e.Mode, found, err, tt.found, tt.mode)
		}
	}
	if e, _, _ := Lookup(context.Background(), trees, root, []byte("a/x")); e.ID != base["a/x"].blob || e.Size != base["a/x"].size {
		t.Errorf("Lookup(a/x) = %+v", e)
	}
}

func TestTreesCacheAndErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	objs := store.NewMem()
	trees := NewTrees(objs, 10)
	if _, err := trees.ReadTree(ctx, hash.ID{1}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing tree: %v", err)
	}
	blobID, stored := object.Encode(object.KindBlob, []byte("not a tree"))
	_ = objs.Put(ctx, blobID, stored)
	if _, err := trees.ReadTree(ctx, blobID); !errors.Is(err, ErrNotTree) {
		t.Errorf("blob as tree: %v", err)
	}
	badID := hash.ID{2}
	_ = objs.Put(ctx, badID, []byte("garbage"))
	if _, err := trees.ReadTree(ctx, badID); !errors.Is(err, object.ErrCorrupt) {
		t.Errorf("garbage as tree: %v", err)
	}
	notCanonical := hash.ID{3}
	_, raw := object.Encode(object.KindTree, []byte{0x80, 0x00})
	_ = objs.Put(ctx, notCanonical, raw)
	if _, err := trees.ReadTree(ctx, notCanonical); !errors.Is(err, object.ErrCorrupt) {
		t.Errorf("non-canonical tree: %v", err)
	}
	// A cached tree is served without the store.
	root := buildFromScratch(t, objs, map[string]file{"f": blob("f")})
	if _, err := trees.ReadTree(ctx, root); err != nil {
		t.Fatal(err)
	}
	empty := NewTrees(store.NewMem(), 10)
	tree, _ := trees.ReadTree(ctx, root)
	empty.Add(root, tree)
	if _, err := empty.ReadTree(ctx, root); err != nil {
		t.Errorf("Add did not cache: %v", err)
	}
}

// ---- property test

// reference applies changes to a flat map, returning ok = false if the change
// list breaks 03 rule 4 (or has duplicate paths).
func reference(m map[string]file, changes []changeset.Change) (map[string]file, bool) {
	out := maps.Clone(m)
	seen := map[string]bool{}
	for _, ch := range changes {
		p := string(ch.Path)
		if seen[p] {
			return nil, false
		}
		seen[p] = true
		if ch.Op == changeset.OpDelete {
			if _, ok := out[p]; !ok {
				return nil, false
			}
			delete(out, p)
		}
	}
	for _, ch := range changes {
		if ch.Op != changeset.OpPut {
			continue
		}
		p := string(ch.Path)
		f := file{mode: ch.Mode, blob: ch.Blob, size: ch.Size}
		if old, ok := m[p]; ok && old.mode == f.mode && old.blob == f.blob {
			if _, stillThere := out[p]; stillThere {
				return nil, false // no-op put
			}
		}
		out[p] = f
	}
	for p := range out {
		for q := range out {
			if strings.HasPrefix(q, p+"/") {
				return nil, false // file/dir clash
			}
		}
	}
	return out, true
}

func TestApplyMatchesFromScratch(t *testing.T) {
	t.Parallel()
	src := rand.NewChaCha8([32]byte{4})
	n := func(k int) int { // small k only
		var b [1]byte
		_, _ = src.Read(b[:])
		return int(b[0]) % k
	}
	names := []string{"a", "b", "c", "a-b", "a.b", "a0", "ab", "\xff"}
	randPath := func() string {
		parts := make([]string, 1+n(4))
		for i := range parts {
			parts[i] = names[n(len(names))]
		}
		return strings.Join(parts, "/")
	}
	randFile := func() file {
		f := blob(fmt.Sprint("content ", n(6)))
		f.mode = []object.Mode{object.ModeFile, object.ModeExec, object.ModeSymlink}[n(3)]
		return f
	}

	valid, invalid := 0, 0
	for seq := range 150 {
		objs := store.NewMem()
		trees := NewTrees(objs, 50) // small cache: exercise store reads too
		flat := map[string]file{}
		root := EmptyTree
		for step := range 25 {
			var changes []changeset.Change
			existing := slices.Sorted(maps.Keys(flat))
			for range 1 + n(6) {
				switch {
				case n(3) == 0 && len(existing) > 0:
					changes = append(changes, del(existing[n(len(existing))]))
				case n(4) == 0 && len(existing) > 0:
					changes = append(changes, put(existing[n(len(existing))], randFile()))
				case n(8) == 0:
					changes = append(changes, del(randPath())) // usually missing
				default:
					changes = append(changes, put(randPath(), randFile()))
				}
			}
			// Occasionally turn a directory into a file with the same name.
			if n(10) == 0 && len(existing) > 0 {
				p := existing[n(len(existing))]
				if i := strings.LastIndexByte(p, '/'); i > 0 {
					dir := p[:i]
					changes = nil
					for _, q := range existing {
						if strings.HasPrefix(q, dir+"/") {
							changes = append(changes, del(q))
						}
					}
					changes = append(changes, put(dir, randFile()))
				}
			}

			want, ok := reference(flat, changes)
			got, _, err := apply(t, trees, objs, root, changes...)
			if !ok {
				invalid++
				if err == nil {
					t.Fatalf("seq %d step %d: Apply accepted an invalid change list %v", seq, step, changes)
				}
				continue
			}
			valid++
			if err != nil {
				t.Fatalf("seq %d step %d: Apply rejected a valid change list: %v", seq, step, err)
			}
			if wantRoot := buildFromScratch(t, objs, want); got != wantRoot {
				t.Fatalf("seq %d step %d: incremental root %s != from-scratch %s", seq, step, got.Short(12), wantRoot.Short(12))
			}
			flat, root = want, got
		}
	}
	if valid < 1000 || invalid < 200 {
		t.Errorf("weak generator: %d valid and %d invalid change lists", valid, invalid)
	}
	t.Logf("%d valid and %d invalid change lists checked", valid, invalid)
}
