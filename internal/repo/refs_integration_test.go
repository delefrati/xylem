//go:build integration

package repo_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/testutil"
	"github.com/delefrati/xylem/internal/treeops"
)

// linear commits n changesets on top of parent (nil for a new root).
func linear(t *testing.T, r *repo.Repo, parent *repo.Changeset, n int, tag string) []*repo.Changeset {
	t.Helper()
	var out []*repo.Changeset
	for i := range n {
		var parents []hash.ID
		if parent != nil {
			parents = []hash.ID{parent.ID}
		}
		parent = repotest.Commit(t, r, parents, fmt.Sprint(tag, i), repotest.Put(t, r, tag, fmt.Sprint(tag, " ", i)))
		out = append(out, parent)
	}
	return out
}

// checkMainline asserts that mainline rows for main are exactly tip's first-parent chain.
func checkMainline(t *testing.T, r *repo.Repo, m *meta.PG, tip *repo.Changeset) {
	t.Helper()
	ctx := context.Background()
	repoID, err := m.LookupRepo(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	cur := tip.Core
	for {
		got, err := m.MainlineAt(ctx, repoID, repo.MainRef, cur.FPDepth)
		if err != nil || got != cur.ChangesetID {
			t.Fatalf("mainline[%d] = %s, %v; want %s", cur.FPDepth, got.Short(8), err, cur.ChangesetID.Short(8))
		}
		if cur.FPDepth == 0 {
			break
		}
		rows, err := r.Source().Core(ctx, []hash.ID{cur.FPParent})
		if err != nil {
			t.Fatal(err)
		}
		cur = rows[cur.FPParent]
	}
	if _, err := m.MainlineAt(ctx, repoID, repo.MainRef, tip.Core.FPDepth+1); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("mainline has rows above the tip depth %d: %v", tip.Core.FPDepth, err)
	}
}

func TestInit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := meta.NewPG(testutil.NewDB(t))
	objs := store.NewMem()
	r, root, err := repo.Init(ctx, m, objs, "proj", repo.Options{Now: repotest.Clock})
	if err != nil {
		t.Fatal(err)
	}
	if root.Core.RootTree != treeops.EmptyTree || len(root.Parents) != 0 || len(root.Changes) != 0 {
		t.Errorf("init root = %+v", root)
	}
	if has, _ := objs.Has(ctx, []hash.ID{treeops.EmptyTree}); !has[0] {
		t.Error("empty tree object not stored")
	}
	ref, err := r.GetRef(ctx, repo.MainRef)
	if err != nil || ref.ID != root.ID || ref.Version != 1 {
		t.Errorf("main = %+v, %v", ref, err)
	}
	if _, _, err := repo.Init(ctx, m, objs, "proj", repo.Options{}); !errors.Is(err, repo.ErrConflict) {
		t.Errorf("second Init = %v, want ErrConflict", err)
	}
}

func TestUpdateRefFastForward(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, m := repotest.NewWithMeta(t)
	c := linear(t, r, nil, 8, "a")
	if _, err := r.UpdateRef(ctx, repo.MainRef, hash.ID{}, c[0].ID, false, "t", "admin"); err != nil {
		t.Fatal(err)
	}
	checkMainline(t, r, m, c[0])
	// One step, then several at once.
	ref, err := r.UpdateRef(ctx, repo.MainRef, c[0].ID, c[1].ID, false, "t", "land")
	if err != nil || ref.Version != 2 {
		t.Fatalf("UpdateRef = %+v, %v", ref, err)
	}
	if _, err := r.UpdateRef(ctx, repo.MainRef, c[1].ID, c[7].ID, false, "t", "land"); err != nil {
		t.Fatal(err)
	}
	checkMainline(t, r, m, c[7])

	// A merge whose first parent is the tip is a fast-forward…
	side := linear(t, r, c[2], 2, "side")
	merge := repotest.Commit(t, r, []hash.ID{c[7].ID, side[1].ID}, "merge", repotest.Put(t, r, "side", "side 1"))
	if _, err := r.UpdateRef(ctx, repo.MainRef, c[7].ID, merge.ID, false, "t", "land"); err != nil {
		t.Fatalf("merge fast-forward: %v", err)
	}
	checkMainline(t, r, m, merge)
	// …but a merge that has the tip only as its second parent is not.
	other := repotest.Commit(t, r, []hash.ID{side[1].ID, merge.ID}, "reverse merge", repotest.Put(t, r, "x", "x"))
	if _, err := r.UpdateRef(ctx, repo.MainRef, merge.ID, other.ID, false, "t", "land"); !errors.Is(err, repo.ErrNotFastForward) {
		t.Errorf("second-parent update = %v, want ErrNotFastForward", err)
	}
	// Backwards is not a fast-forward either.
	if _, err := r.UpdateRef(ctx, repo.MainRef, merge.ID, c[3].ID, false, "t", "land"); !errors.Is(err, repo.ErrNotFastForward) {
		t.Errorf("backwards update = %v, want ErrNotFastForward", err)
	}
	checkMainline(t, r, m, merge)
}

func TestUpdateRefForceRewind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, m := repotest.NewWithMeta(t)
	c := linear(t, r, nil, 6, "a")
	if _, err := r.UpdateRef(ctx, repo.MainRef, hash.ID{}, c[5].ID, false, "t", "admin"); err != nil {
		t.Fatal(err)
	}
	side := linear(t, r, c[2], 4, "side") // depths 3..6
	if _, err := r.UpdateRef(ctx, repo.MainRef, c[5].ID, side[3].ID, true, "t", "admin"); err != nil {
		t.Fatalf("forced update: %v", err)
	}
	checkMainline(t, r, m, side[3])
	repoID, _ := m.LookupRepo(ctx, "default")
	depths, err := m.MainlineLookup(ctx, repoID, repo.MainRef, []hash.ID{c[3].ID, c[4].ID, c[5].ID})
	if err != nil || len(depths) != 0 {
		t.Errorf("rewound changesets still on mainline: %v, %v", depths, err)
	}
	// Rewind backwards (to an ancestor) and to an unrelated history.
	if _, err := r.UpdateRef(ctx, repo.MainRef, side[3].ID, c[1].ID, true, "t", "admin"); err != nil {
		t.Fatal(err)
	}
	checkMainline(t, r, m, c[1])
	unrelated := linear(t, r, nil, 3, "u")
	if _, err := r.UpdateRef(ctx, repo.MainRef, c[1].ID, unrelated[2].ID, true, "t", "admin"); err != nil {
		t.Fatal(err)
	}
	checkMainline(t, r, m, unrelated[2])
}

func TestUpdateRefErrorsAndOtherRefs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, m := repotest.NewWithMeta(t)
	c := linear(t, r, nil, 3, "a")
	if _, err := r.UpdateRef(ctx, repo.MainRef, hash.ID{}, c[0].ID, false, "t", "admin"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name             string
		ref              string
		expected, target hash.ID
		want             error
	}{
		{"bad name", "Main", c[0].ID, c[1].ID, repo.ErrInvalid},
		{"bad name slash", "/main", c[0].ID, c[1].ID, repo.ErrInvalid},
		{"stale expected", repo.MainRef, c[1].ID, c[2].ID, repo.ErrConflict},
		{"create existing", repo.MainRef, hash.ID{}, c[2].ID, repo.ErrConflict},
		{"unknown target", repo.MainRef, c[0].ID, hash.ID{7}, repo.ErrNotFound},
		{"unknown ref", "nope", c[0].ID, c[1].ID, repo.ErrNotFound},
	}
	for _, tt := range tests {
		if _, err := r.UpdateRef(ctx, tt.ref, tt.expected, tt.target, false, "t", "admin"); !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.want)
		}
	}
	// Other refs move but have no mainline.
	if _, err := r.UpdateRef(ctx, "release/2026.10", hash.ID{}, c[2].ID, false, "t", "admin"); err != nil {
		t.Fatal(err)
	}
	repoID, _ := m.LookupRepo(ctx, "default")
	if _, err := m.MainlineAt(ctx, repoID, "release/2026.10", 0); !errors.Is(err, meta.ErrNotFound) {
		t.Errorf("non-main ref got mainline rows: %v", err)
	}
	if ref, err := r.GetRef(ctx, "release/2026.10"); err != nil || ref.ID != c[2].ID {
		t.Errorf("GetRef = %+v, %v", ref, err)
	}
	if _, err := r.GetRef(ctx, "nope"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("GetRef(nope) = %v", err)
	}
}

func TestUpdateRefConcurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, m := repotest.NewWithMeta(t)
	base := linear(t, r, nil, 1, "base")[0]
	if _, err := r.UpdateRef(ctx, repo.MainRef, hash.ID{}, base.ID, false, "t", "admin"); err != nil {
		t.Fatal(err)
	}
	const n = 10
	targets := make([]*repo.Changeset, n)
	for i := range n {
		targets[i] = linear(t, r, base, 1, fmt.Sprint("t", i))[0]
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { _, errs[i] = r.UpdateRef(ctx, repo.MainRef, base.ID, targets[i].ID, false, "t", "land") })
	}
	wg.Wait()
	won := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if won >= 0 {
				t.Fatalf("two concurrent updates succeeded")
			}
			won = i
		case !errors.Is(err, repo.ErrConflict):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won < 0 {
		t.Fatal("no concurrent update succeeded")
	}
	checkMainline(t, r, m, targets[won])
}
