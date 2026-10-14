package treeops

import (
	"context"
	"errors"
	"slices"
	"sort"
	"testing"

	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/store"
)

func TestComparePathsDFS(t *testing.T) {
	t.Parallel()
	ordered := []string{"a/x", "a/y/z", "a-b", "a.b", "a0", "ab", "b", "b/c"}
	// b (a file) and b/c can't coexist in one tree, but the order is still defined.
	for i, a := range ordered {
		for j, b := range ordered {
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := ComparePathsDFS([]byte(a), []byte(b)); got != want {
				t.Errorf("ComparePathsDFS(%q, %q) = %d, want %d", a, b, got, want)
			}
		}
	}
}

func TestWalkFiles(t *testing.T) {
	t.Parallel()
	objs := store.NewMem()
	files := map[string]file{
		"a/x": blob("1"), "a/y/z": blob("2"), "a-b": blob("3"), "a.b/c": blob("4"),
		"ab": blob("5"), "b/c/d/e": blob("6"), "b/f": blob("7"), "z": blob("8"),
	}
	root := buildFromScratch(t, objs, files)
	trees := NewTrees(objs, 100)
	ctx := context.Background()
	collect := func(after string, limit int) []string {
		var out []string
		err := WalkFiles(ctx, trees, root, []byte(after), func(p []byte, e object.Entry) error {
			if f := files[string(p)]; f.blob != e.ID {
				t.Errorf("%q has the wrong id", p)
			}
			out = append(out, string(p))
			if len(out) == limit {
				return ErrStop
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	var want []string
	for p := range files {
		want = append(want, p)
	}
	sort.Slice(want, func(i, j int) bool { return ComparePathsDFS([]byte(want[i]), []byte(want[j])) < 0 })
	if got := collect("", 0); !slices.Equal(got, want) {
		t.Fatalf("full walk = %v, want %v", got, want)
	}
	// Resuming after every position yields the rest.
	for i, p := range want {
		if got := collect(p, 0); !slices.Equal(got, want[i+1:]) {
			t.Errorf("after %q = %v, want %v", p, got, want[i+1:])
		}
	}
	// Resume points that are directories or absent paths work too.
	if got := collect("a", 0); !slices.Equal(got, want[slices.Index(want, "a-b"):]) {
		t.Errorf("after dir a = %v", got)
	}
	if got := collect("b/c", 0); got[0] != "b/f" {
		t.Errorf("after dir b/c = %v", got)
	}
	if got := collect("aa", 0); got[0] != "ab" {
		t.Errorf("after absent aa = %v", got)
	}
	// Paging by 3 reassembles the full walk.
	var paged []string
	last := ""
	for {
		page := collect(last, 3)
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		last = page[len(page)-1]
	}
	if !slices.Equal(paged, want) {
		t.Errorf("paged walk = %v", paged)
	}
	// Errors propagate; ErrStop does not.
	boom := errors.New("boom")
	if err := WalkFiles(ctx, trees, root, nil, func([]byte, object.Entry) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("error not propagated: %v", err)
	}
}
