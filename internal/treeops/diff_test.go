package treeops

import (
	"context"
	"errors"
	"maps"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
	"github.com/delefrati/xylem/internal/store"
)

// countingReader counts tree reads.
type countingReader struct {
	TreeReader
	reads atomic.Int64
}

func (c *countingReader) ReadTree(ctx context.Context, id hash.ID) (*object.Tree, error) {
	c.reads.Add(1)
	return c.TreeReader.ReadTree(ctx, id)
}

// referenceDiff compares two flat maps, filtered by prefixes, in DFS order.
func referenceDiff(a, b map[string]file, prefixes []string) []string {
	var out []string
	keep := func(p string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, pre := range prefixes {
			if paths.IsUnder([]byte(p), []byte(pre)) {
				return true
			}
		}
		return false
	}
	for p := range a {
		if fb, ok := b[p]; (!ok || fb != a[p]) && keep(p) {
			out = append(out, p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok && keep(p) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return ComparePathsDFS([]byte(out[i]), []byte(out[j])) < 0 })
	return slices.Compact(out)
}

func TestDiffMatchesReference(t *testing.T) {
	t.Parallel()
	r := rand.NewChaCha8([32]byte{9})
	n := func(k int) int { return int(r.Uint64()>>34) % k }
	names := []string{"a", "b", "a-b", "a.b", "ab"}
	randPath := func() string {
		parts := make([]string, 1+n(3))
		for i := range parts {
			parts[i] = names[n(len(names))]
		}
		return strings.Join(parts, "/")
	}
	valid := func(m map[string]file) bool { // no path is both a file and a directory
		for p := range m {
			for q := range m {
				if strings.HasPrefix(q, p+"/") {
					return false
				}
			}
		}
		return true
	}
	randMap := func() map[string]file {
		for {
			m := map[string]file{}
			for range n(12) {
				f := blob(names[n(len(names))])
				if n(4) == 0 {
					f.mode = object.ModeExec
				}
				m[randPath()] = f
			}
			if valid(m) {
				return m
			}
		}
	}
	ctx := context.Background()
	for round := range 400 {
		objs := store.NewMem()
		a := randMap()
		b := maps.Clone(a)
		if round%2 == 0 { // mostly similar trees
			for range 1 + n(3) {
				p := randPath()
				if n(2) == 0 {
					delete(b, p)
				} else {
					b[p] = blob("changed")
				}
			}
			if !valid(b) {
				continue
			}
		} else {
			b = randMap()
		}
		ra, rb := buildFromScratch(t, objs, a), buildFromScratch(t, objs, b)
		var prefixes []string
		if round%3 == 0 {
			prefixes = []string{names[n(len(names))], randPath()}
		}
		var pre [][]byte
		for _, p := range prefixes {
			pre = append(pre, []byte(p))
		}
		changes, err := Diff(ctx, NewTrees(objs, 100), ra, rb, pre, 0)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, c := range changes {
			got = append(got, string(c.Path))
			old, newF := a[string(c.Path)], b[string(c.Path)]
			if c.Old.ID != old.blob || c.New.ID != newF.blob || c.Old.Mode != old.mode || c.New.Mode != newF.mode {
				t.Fatalf("round %d: %q entries %v/%v, want %v/%v", round, c.Path, c.Old, c.New, old, newF)
			}
		}
		if want := referenceDiff(a, b, prefixes); !slices.Equal(got, want) {
			t.Fatalf("round %d (prefixes %q):\n a=%v\n b=%v\n got  %q\n want %q", round, prefixes, a, b, got, want)
		}
	}
}

func TestDiffSkipsEqualSubtreesAndBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	objs := store.NewMem()
	files := map[string]file{}
	for _, d := range []string{"d1", "d2", "d3", "d4"} {
		for _, f := range []string{"x", "y", "z/deep/file"} {
			files[d+"/"+f] = blob(d + f)
		}
	}
	a := buildFromScratch(t, objs, files)
	files["d3/z/deep/file"] = blob("changed")
	b := buildFromScratch(t, objs, files)
	cr := &countingReader{TreeReader: NewTrees(objs, 100)}
	changes, err := Diff(ctx, cr, a, b, nil, 0)
	if err != nil || len(changes) != 1 || string(changes[0].Path) != "d3/z/deep/file" {
		t.Fatalf("Diff = %v, %v", changes, err)
	}
	// root, d3, z, deep on each side: 8 reads; untouched d1, d2, d4 are skipped.
	if reads := cr.reads.Load(); reads != 8 {
		t.Errorf("Diff read %d trees, want 8", reads)
	}
	if got, _ := Diff(ctx, cr, a, a, nil, 0); len(got) != 0 {
		t.Error("diff of a tree with itself is not empty")
	}
	if _, err := Diff(ctx, NewTrees(objs, 100), a, b, nil, 5); !errors.Is(err, ErrTooLarge) {
		t.Errorf("budget 5: %v", err)
	}
	if _, err := Diff(ctx, NewTrees(objs, 100), a, b, nil, 100); err != nil {
		t.Errorf("budget 100: %v", err)
	}
	// The zero id is the empty tree: everything is added.
	all, err := Diff(ctx, NewTrees(objs, 100), hash.ID{}, b, nil, 0)
	if err != nil || len(all) != len(files) {
		t.Errorf("diff from empty: %d changes, %v", len(all), err)
	}
}
