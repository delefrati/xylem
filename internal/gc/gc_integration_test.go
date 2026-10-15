//go:build integration

package gc_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/gc"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/importer"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/testutil"
)

func dryRun(t *testing.T, env *repotest.Env, minAge time.Duration) (*gc.Report, []gc.Orphan) {
	t.Helper()
	var orphans []gc.Orphan
	rep, err := gc.DryRun(context.Background(), env.Meta, env.Objects, gc.Options{MinAge: minAge}, func(o gc.Orphan) error {
		orphans = append(orphans, o)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Missing != 0 {
		t.Errorf("%d reachable objects missing from the store", rep.Missing)
	}
	return rep, orphans
}

// TestDryRunAfterConcurrentLands is M9.4's acceptance: on branchy, after a
// concurrent land workload (batcher off and on, plus a batched land that
// fails before its CAS) and blob uploads that are never committed, the only
// orphans are trees from aborted land attempts and those blobs.
func TestDryRunAfterConcurrentLands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := repotest.NewEnv(t, store.NewMem())
	src, err := gitsource.Open(ctx, testutil.GitFixture(t, "branchy"), gitsource.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	im, err := importer.New(env.Repo, src, importer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := im.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// Right after the import everything is reachable.
	rep, _ := dryRun(t, env, 0)
	if rep.Orphans.Objects != 0 || rep.Reachable == 0 || rep.Stored.Objects != rep.Reachable || rep.Repos != 1 {
		t.Fatalf("after import: %+v", rep)
	}

	batched, err := repo.Open(ctx, env.Meta, env.Objects, "default", repo.Options{Now: repotest.Clock, LandBatchWindow: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	base, err := env.Repo.GetRef(ctx, repo.MainRef)
	if err != nil {
		t.Fatal(err)
	}
	// Concurrent lands on disjoint new files, all based on the import's tip,
	// through the unbatched repo and the batching one.
	g, gctx := errgroup.WithContext(ctx)
	for w := range 8 {
		r := []*repo.Repo{env.Repo, batched}[w%2]
		g.Go(func() error {
			for i := range 10 {
				path := fmt.Sprintf("gc/w%d/f%d.txt", w, i)
				x := put(gctx, t, r, base.ID, path)
				if _, err := r.Land(gctx, []hash.ID{x}, repo.LandOptions{MaxAttempts: 1000}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	// A batched land that fails before its CAS: its rebased trees were
	// stored, its changesets never inserted.
	crash := errors.New("crash")
	x := put(ctx, t, batched, base.ID, "gc/crashed.txt")
	if _, err := batched.Land(ctx, []hash.ID{x}, repo.LandOptions{BeforeCAS: func(context.Context, int) error { return crash }}); !errors.Is(err, crash) {
		t.Fatalf("crashed land: %v", err)
	}
	// Blobs uploaded and never committed.
	uploaded := map[hash.ID]bool{}
	for i := range 5 {
		id, stored := object.Encode(object.KindBlob, []byte(fmt.Sprintf("uploaded, never committed %d\n", i)))
		if err := env.Objects.Put(ctx, id, stored); err != nil {
			t.Fatal(err)
		}
		uploaded[id] = true
	}
	// A derived value stored as an object is reachable through value_ref.
	derivedID, derivedStored := object.Encode(object.KindDerived, []byte("a large derived value"))
	if err := env.Objects.Put(ctx, derivedID, derivedStored); err != nil {
		t.Fatal(err)
	}
	if err := env.Meta.PutDerived(ctx, 1, meta.DerivedKey{Kind: "test", Version: 1, Changeset: base.ID, Key: []byte("k")}, nil, derivedID); err != nil {
		t.Fatal(err)
	}

	rep, orphans := dryRun(t, env, 0)
	trees := 0
	for _, o := range orphans {
		switch {
		case o.Kind == object.KindTree:
			trees++
		case o.Kind == object.KindBlob && uploaded[o.ID]:
			delete(uploaded, o.ID)
		default:
			t.Errorf("unexpected orphan %s (%s, %d bytes)", o.ID.Short(12), o.Kind, o.Size)
		}
	}
	if len(uploaded) != 0 {
		t.Errorf("%d uploaded-never-committed blobs not reported", len(uploaded))
	}
	if trees == 0 {
		t.Error("no orphan trees from the crashed batched land")
	}
	if rep.ByKind[object.KindTree].Objects != trees || rep.ByKind[object.KindBlob].Objects != 5 || rep.Orphans.Objects != trees+5 {
		t.Errorf("report %+v by kind tree=%+v blob=%+v", rep, rep.ByKind[object.KindTree], rep.ByKind[object.KindBlob])
	}
	t.Logf("after the workload: %d reachable, %d orphans (%d trees, 5 blobs), %d bytes", rep.Reachable, rep.Orphans.Objects, trees, rep.Orphans.Bytes)

	// Within the grace period nothing is an orphan.
	rep, orphans = dryRun(t, env, gc.DefaultMinAge)
	if rep.Orphans.Objects != 0 || len(orphans) != 0 || rep.Young.Objects != trees+5 {
		t.Errorf("with the default grace period: %+v", rep)
	}
}

// put stores a one-file changeset on parent.
func put(ctx context.Context, t *testing.T, r *repo.Repo, parent hash.ID, path string) hash.ID {
	t.Helper()
	content := path + "\n"
	id, stored := object.Encode(object.KindBlob, []byte(content))
	if err := r.Objects().Put(ctx, id, stored); err != nil {
		t.Error(err)
		return hash.ID{}
	}
	cs, _, err := r.CreateChangeset(ctx, repo.CreateRequest{
		Parents: []hash.ID{parent}, Author: repotest.Author, Message: []byte(path),
		Changes: []changeset.Change{{Path: []byte(path), Op: changeset.OpPut, Mode: object.ModeFile, Size: uint64(len(content)), Blob: id}},
	}, repo.CreateOptions{})
	if err != nil {
		t.Error(err)
	}
	return cs.ID
}

func TestDryRunNeedsAListableStore(t *testing.T) {
	t.Parallel()
	env := repotest.NewEnv(t, store.NewMem())
	if _, err := gc.DryRun(context.Background(), env.Meta, struct{ store.ObjectStore }{env.Objects}, gc.Options{}, nil); !errors.Is(err, gc.ErrNotListable) {
		t.Errorf("unlistable store: %v", err)
	}
}
