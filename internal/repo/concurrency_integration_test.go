//go:build integration

package repo_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/treeops"
)

// TestLandConcurrencyDisjoint is 07 §Concurrency test requirement 1: 20
// goroutines land 50 single-changeset stacks each, on disjoint paths, with
// max_attempts = 1000. Every land succeeds, main gains exactly 1,000
// mainline entries and the final tree has every change.
func TestLandConcurrencyDisjoint(t *testing.T) {
	t.Parallel()
	forEachLandMode(t, landConcurrencyDisjoint)
}

func landConcurrencyDisjoint(t *testing.T, window time.Duration) {
	const workers, perWorker = 20, 50
	ctx := context.Background()
	env, root := landEnvWindow(t, window)
	r := env.Repo
	start := mainTip(t, r)
	startRow, err := r.Changeset(ctx, start.ID)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	totalAttempts := 0
	g, gctx := errgroup.WithContext(ctx)
	for w := range workers {
		g.Go(func() error {
			for i := range perWorker {
				path := fmt.Sprintf("w%02d/f%02d.txt", w, i)
				// Each stack is based on the old root, so every land rebases.
				x, err := commitErr(gctx, r, root.ID, path, path+"\n")
				if err != nil {
					return err
				}
				res, err := r.Land(gctx, []hash.ID{x}, repo.LandOptions{MaxAttempts: 1000})
				if err != nil {
					return fmt.Errorf("land %s: %w", path, err)
				}
				if len(res.Conflicts) > 0 || len(res.Landed) != 1 {
					return fmt.Errorf("land %s: %+v", path, res)
				}
				mu.Lock()
				totalAttempts += res.Attempts
				mu.Unlock()
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d lands, %d CAS attempts (%.2f per land)", workers*perWorker, totalAttempts, float64(totalAttempts)/(workers*perWorker))

	end := mainTip(t, r)
	tip, err := r.Changeset(ctx, end.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := tip.Core.FPDepth - startRow.Core.FPDepth; got != workers*perWorker {
		t.Errorf("main advanced %d first-parent steps, want %d", got, workers*perWorker)
	}
	// Exactly 1,000 new mainline rows, all on the first-parent chain of the tip.
	var rows int64
	if err := env.Pool.QueryRow(ctx, `SELECT count(*) FROM mainline WHERE fp_depth > $1`, startRow.Core.FPDepth).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != workers*perWorker {
		t.Errorf("%d new mainline rows, want %d", rows, workers*perWorker)
	}
	files := map[string]bool{}
	err = treeops.WalkFiles(ctx, r.Trees(), tip.Core.RootTree, nil, func(p []byte, _ object.Entry) error {
		files[string(p)] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for w := range workers {
		for i := range perWorker {
			if p := fmt.Sprintf("w%02d/f%02d.txt", w, i); !files[p] {
				t.Errorf("final tree lacks %s", p)
			}
		}
	}
	verifyRepo(t, r)
}

// TestLandConcurrencySameFile is requirement 2 for the same region: two
// stacks rewrite the same line concurrently; exactly one lands and the other
// gets a content conflict.
func TestLandConcurrencySameFile(t *testing.T) {
	t.Parallel()
	forEachLandMode(t, landConcurrencySameFile)
}

func landConcurrencySameFile(t *testing.T, window time.Duration) {
	ctx := context.Background()
	env, _ := landEnvWindow(t, window)
	r := env.Repo
	for round := range 10 {
		tip := mainTip(t, r)
		stacks := make([]hash.ID, 2)
		for i := range stacks {
			content := fmt.Sprintf("round %d, writer %d\n", round, i)
			stacks[i] = repotest.Commit(t, r, []hash.ID{tip.ID}, content, repotest.Put(t, r, "a.txt", content)).ID
		}
		results := make([]*repo.LandResult, 2)
		var g errgroup.Group
		ready := make(chan struct{})
		for i := range stacks {
			g.Go(func() error {
				<-ready
				var err error
				results[i], err = r.Land(ctx, []hash.ID{stacks[i]}, repo.LandOptions{MaxAttempts: 100})
				return err
			})
		}
		close(ready)
		if err := g.Wait(); err != nil {
			t.Fatal(err)
		}
		landed, conflicted := 0, 0
		for _, res := range results {
			switch {
			case len(res.Landed) == 1 && len(res.Conflicts) == 0:
				landed++
			case len(res.Conflicts) == 1 && res.Conflicts[0].Kind == repo.ConflictContent && string(res.Conflicts[0].Path) == "a.txt":
				conflicted++
			}
		}
		if landed != 1 || conflicted != 1 {
			t.Fatalf("round %d: %d landed, %d conflicted: %+v %+v", round, landed, conflicted, results[0], results[1])
		}
	}
	verifyRepo(t, r)
}

// TestLandConcurrencySameFileRegions is requirement 2 with line merge (M7):
// two stacks edit different regions of the same file concurrently; both land
// and main has both edits.
func TestLandConcurrencySameFileRegions(t *testing.T) {
	t.Parallel()
	forEachLandMode(t, landConcurrencySameFileRegions)
}

func landConcurrencySameFileRegions(t *testing.T, window time.Duration) {
	ctx := context.Background()
	env, root := landEnvWindow(t, window)
	r := env.Repo
	text := func(top, bottom string) string {
		return top + "\n2\n3\n4\n5\n6\n7\n8\n9\n" + bottom + "\n"
	}
	start := repotest.Commit(t, r, []hash.ID{root.ID}, "add f.txt", repotest.Put(t, r, "f.txt", text("top -1", "bottom -1")))
	if _, err := r.UpdateRef(ctx, repo.MainRef, root.ID, start.ID, false, "test", "admin"); err != nil {
		t.Fatal(err)
	}
	for round := range 10 {
		tip := mainTip(t, r)
		prevTop, prevBottom := fmt.Sprintf("top %d", round-1), fmt.Sprintf("bottom %d", round-1)
		top, bottom := fmt.Sprintf("top %d", round), fmt.Sprintf("bottom %d", round)
		stacks := []hash.ID{
			repotest.Commit(t, r, []hash.ID{tip.ID}, top, repotest.Put(t, r, "f.txt", text(top, prevBottom))).ID,
			repotest.Commit(t, r, []hash.ID{tip.ID}, bottom, repotest.Put(t, r, "f.txt", text(prevTop, bottom))).ID,
		}
		results := make([]*repo.LandResult, 2)
		var g errgroup.Group
		ready := make(chan struct{})
		for i := range stacks {
			g.Go(func() error {
				<-ready
				var err error
				results[i], err = r.Land(ctx, []hash.ID{stacks[i]}, repo.LandOptions{MaxAttempts: 100})
				return err
			})
		}
		close(ready)
		if err := g.Wait(); err != nil {
			t.Fatal(err)
		}
		for i, res := range results {
			if len(res.Conflicts) != 0 || len(res.Landed) != 1 {
				t.Fatalf("round %d, writer %d: %+v", round, i, res)
			}
		}
		tipCS, err := r.Changeset(ctx, mainTip(t, r).ID)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := content(t, r, tipCS, "f.txt"); got != text(top, bottom) {
			t.Fatalf("round %d: f.txt = %q", round, got)
		}
	}
	verifyRepo(t, r)
}

// TestLandConcurrencyCrash is requirement 3: a failure between the rebased
// changeset's insert and the CAS leaves at most an orphan draft; the ref and
// the mainline stay consistent, and a later land succeeds.
func TestLandConcurrencyCrash(t *testing.T) {
	t.Parallel()
	forEachLandMode(t, landConcurrencyCrash)
}

func landConcurrencyCrash(t *testing.T, window time.Duration) {
	ctx := context.Background()
	env, root := landEnvWindow(t, window)
	r := env.Repo
	m1 := repotest.Commit(t, r, []hash.ID{root.ID}, "m1", repotest.Put(t, r, "README", "moved\n"))
	if _, err := r.UpdateRef(ctx, repo.MainRef, root.ID, m1.ID, false, "test", "admin"); err != nil {
		t.Fatal(err)
	}
	x := repotest.Commit(t, r, []hash.ID{root.ID}, "x", repotest.Put(t, r, "x.txt", "x\n"))
	before := mainTip(t, r)
	var countBefore int64
	if err := env.Pool.QueryRow(ctx, `SELECT count(*) FROM changesets`).Scan(&countBefore); err != nil {
		t.Fatal(err)
	}
	crash := errors.New("simulated crash")
	_, err := r.Land(ctx, []hash.ID{x.ID}, repo.LandOptions{BeforeCAS: func(context.Context, int) error { return crash }})
	if !errors.Is(err, crash) {
		t.Fatalf("land = %v, want the simulated crash", err)
	}
	after := mainTip(t, r)
	if after != before {
		t.Errorf("main moved: %+v → %+v", before, after)
	}
	var countAfter int64
	if err := env.Pool.QueryRow(ctx, `SELECT count(*) FROM changesets`).Scan(&countAfter); err != nil {
		t.Fatal(err)
	}
	// Unbatched, the rebased draft was inserted before the CAS; the batcher
	// inserts changesets in the CAS's transaction, so it leaves none.
	if want := map[bool]int64{false: 1, true: 0}[window > 0]; countAfter-countBefore != want {
		t.Errorf("%d changesets created by the crashed land, want %d", countAfter-countBefore, want)
	}
	verifyRepo(t, r)

	res, err := r.Land(ctx, []hash.ID{x.ID}, repo.LandOptions{})
	if err != nil || len(res.Landed) != 1 {
		t.Fatalf("land after the crash = %+v, %v", res, err)
	}
	verifyRepo(t, r)
}

// forEachLandMode runs a concurrency test with the land batcher off and on
// (M9.3: the suite must pass both ways).
func forEachLandMode(t *testing.T, test func(t *testing.T, window time.Duration)) {
	for _, window := range []time.Duration{0, 20 * time.Millisecond} {
		t.Run(fmt.Sprintf("batch=%v", window), func(t *testing.T) {
			t.Parallel()
			test(t, window)
		})
	}
}

// commitErr creates a single-file changeset on parent without failing the
// test from a worker goroutine.
func commitErr(ctx context.Context, r *repo.Repo, parent hash.ID, path, content string) (hash.ID, error) {
	id, stored := object.Encode(object.KindBlob, []byte(content))
	if err := r.Objects().Put(ctx, id, stored); err != nil {
		return hash.ID{}, err
	}
	cs, _, err := r.CreateChangeset(ctx, repo.CreateRequest{
		Parents: []hash.ID{parent}, Author: repotest.Author, Message: []byte(path),
		Changes: []changeset.Change{{Path: []byte(path), Op: changeset.OpPut, Mode: object.ModeFile, Size: uint64(len(content)), Blob: id}},
	}, repo.CreateOptions{})
	if err != nil {
		return hash.ID{}, err
	}
	return cs.ID, nil
}
