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

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
)

// TestLandBatcherTwoServers: two batching Repos on one database (two
// servers) race for main; lost CASes are retried with the whole batch, and
// every land arrives exactly once.
func TestLandBatcherTwoServers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, root := landEnvWindow(t, 5*time.Millisecond)
	other, err := repo.Open(ctx, env.Meta, env.Objects, "default", repo.Options{
		Now: repotest.Clock, LandBatchWindow: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	servers := []*repo.Repo{env.Repo, other}
	start := mainTip(t, env.Repo)
	startRow, err := env.Repo.Changeset(ctx, start.ID)
	if err != nil {
		t.Fatal(err)
	}
	const workers, perWorker = 16, 10
	var mu sync.Mutex
	var originals [][]byte
	g, gctx := errgroup.WithContext(ctx)
	for w := range workers {
		g.Go(func() error {
			r := servers[w%2]
			for i := range perWorker {
				path := fmt.Sprintf("w%02d/f%02d.txt", w, i)
				x, err := commitErr(gctx, r, root.ID, path, path+"\n")
				if err != nil {
					return err
				}
				mu.Lock()
				originals = append(originals, x[:])
				mu.Unlock()
				res, err := r.Land(gctx, []hash.ID{x}, repo.LandOptions{MaxAttempts: 1000, Actor: fmt.Sprintf("w%02d", w)})
				if err != nil {
					return fmt.Errorf("land %s: %w", path, err)
				}
				if len(res.Landed) != 1 || res.NewTip != res.Landed[0] || res.Attempts < 1 {
					return fmt.Errorf("land %s: %+v", path, res)
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	tip, err := env.Repo.Changeset(ctx, mainTip(t, env.Repo).ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := tip.Core.FPDepth - startRow.Core.FPDepth; got != workers*perWorker {
		t.Errorf("main advanced %d, want %d", got, workers*perWorker)
	}
	var rows, orphans int64
	if err := env.Pool.QueryRow(ctx, `SELECT count(*) FROM mainline WHERE fp_depth > $1`, startRow.Core.FPDepth).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != workers*perWorker {
		t.Errorf("%d new mainline rows, want %d", rows, workers*perWorker)
	}
	// Lost CASes write nothing: every changeset is an original stack entry
	// or on main.
	if err := env.Pool.QueryRow(ctx, `SELECT count(*) FROM changesets
		WHERE id NOT IN (SELECT changeset_id FROM mainline) AND NOT id = ANY($1)`, originals).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d orphan changesets from lost CASes, want 0", orphans)
	}
	verifyRepo(t, env.Repo)
}

// TestLandBatcherRequests: within one batch, a bad request fails alone, a
// stack conflicting with an earlier one in the batch gets the conflict, a
// stack already on main is skipped, and a cancelled caller gets its error.
func TestLandBatcherRequests(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, root := landEnvWindow(t, 50*time.Millisecond)
	r := env.Repo
	a := repotest.Commit(t, r, []hash.ID{root.ID}, "a", repotest.Put(t, r, "README", "a\n"))
	b := repotest.Commit(t, r, []hash.ID{root.ID}, "b", repotest.Put(t, r, "README", "b\n"))
	c := repotest.Commit(t, r, []hash.ID{root.ID}, "c", repotest.Put(t, r, "c.txt", "c\n"))
	type out struct {
		res *repo.LandResult
		err error
	}
	land := func(ctx context.Context, stack ...hash.ID) chan out {
		ch := make(chan out, 1)
		go func() {
			res, err := r.Land(ctx, stack, repo.LandOptions{})
			ch <- out{res, err}
		}()
		time.Sleep(2 * time.Millisecond) // keep the order within the batch
		return ch
	}
	cancelled, cancel := context.WithCancel(ctx)
	first := land(ctx, a.ID)
	bad := land(ctx, hash.ID{1})
	second := land(ctx, b.ID)
	gone := land(cancelled, c.ID)
	cancel()
	third := land(ctx, c.ID)

	if o := <-first; o.err != nil || len(o.res.Landed) != 1 {
		t.Errorf("first: %+v, %v", o.res, o.err)
	}
	if o := <-bad; !errors.Is(o.err, repo.ErrNotFound) {
		t.Errorf("unknown stack: %v", o.err)
	}
	if o := <-second; o.err != nil || len(o.res.Conflicts) != 1 || o.res.Conflicting != b.ID {
		t.Errorf("conflicting with the first: %+v, %v", o.res, o.err)
	}
	if o := <-gone; !errors.Is(o.err, context.Canceled) {
		t.Errorf("cancelled: %v", o.err)
	}
	o := <-third
	if o.err != nil || len(o.res.Landed) != 1 {
		t.Fatalf("third: %+v, %v", o.res, o.err)
	}
	if tip := mainTip(t, r); tip.ID != o.res.NewTip {
		t.Errorf("main %s, want the batch's last land %s", tip.ID.Short(8), o.res.NewTip.Short(8))
	}
	// Landing c again: already there, skipped, nothing moves.
	again, err := r.Land(ctx, []hash.ID{c.ID}, repo.LandOptions{})
	if err != nil || len(again.Skipped) != 1 || len(again.Landed) != 0 || again.NewTip != o.res.NewTip {
		t.Errorf("land again: %+v, %v", again, err)
	}
	verifyRepo(t, r)
}
