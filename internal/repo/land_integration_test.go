//go:build integration

package repo_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/store"
)

// landEnv is a repo whose main points at a root with README and a.txt.
func landEnv(t *testing.T) (*repotest.Env, *repo.Changeset) {
	t.Helper()
	env := repotest.NewEnv(t, store.NewMem())
	root := repotest.Commit(t, env.Repo, nil, "root", repotest.Put(t, env.Repo, "README", "r\n"), repotest.Put(t, env.Repo, "a.txt", "a\n"))
	if _, err := env.Repo.UpdateRef(context.Background(), repo.MainRef, hash.ID{}, root.ID, false, "test", "admin"); err != nil {
		t.Fatal(err)
	}
	return env, root
}

func mainTip(t *testing.T, r *repo.Repo) repo.Ref {
	t.Helper()
	ref, err := r.GetRef(context.Background(), repo.MainRef)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func verifyRepo(t *testing.T, r *repo.Repo) {
	t.Helper()
	rep, err := r.Verify(context.Background(), repo.VerifyOptions{Objects: true})
	if err != nil || len(rep.Violations) != 0 {
		t.Fatalf("verify: %v %+v", err, rep)
	}
}

func TestLand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, root := landEnv(t)
	r := env.Repo
	P := func(path, content string) changeset.Change { return repotest.Put(t, r, path, content) }

	// main moves on after the stack was started.
	m1 := repotest.Commit(t, r, []hash.ID{root.ID}, "m1", P("README", "r2\n"))
	if _, err := r.UpdateRef(ctx, repo.MainRef, root.ID, m1.ID, false, "test", "admin"); err != nil {
		t.Fatal(err)
	}
	x1 := repotest.Commit(t, r, []hash.ID{root.ID}, "x1", P("a.txt", "a1\n"))
	x2 := repotest.Commit(t, r, []hash.ID{x1.ID}, "x2", P("b.txt", "b\n"))

	res, err := r.Land(ctx, []hash.ID{x1.ID, x2.ID}, repo.LandOptions{Actor: "ada"})
	if err != nil || len(res.Conflicts) != 0 || len(res.Landed) != 2 || res.Attempts != 1 {
		t.Fatalf("land = %+v, %v", res, err)
	}
	ref := mainTip(t, r)
	if ref.ID != res.NewTip || res.NewTip != res.Landed[1] {
		t.Errorf("main at %v, land says %v", ref.ID, res.NewTip)
	}
	depths, err := r.MainlineDepths(ctx, append([]hash.ID{m1.ID}, res.Landed...))
	if err != nil || depths[m1.ID] != 1 || depths[res.Landed[0]] != 2 || depths[res.Landed[1]] != 3 {
		t.Errorf("mainline depths %v, %v", depths, err)
	}
	var reason, actor string
	if err := env.Pool.QueryRow(ctx, `SELECT reason, actor FROM ref_log WHERE name = 'main' ORDER BY version DESC LIMIT 1`).Scan(&reason, &actor); err != nil || reason != "land" || actor != "ada" {
		t.Errorf("ref_log %q %q, %v", reason, actor, err)
	}

	// Landing the same stack again: everything is already on main, no CAS.
	again, err := r.Land(ctx, []hash.ID{x1.ID, x2.ID}, repo.LandOptions{})
	if err != nil || len(again.Skipped) != 2 || len(again.Landed) != 0 || again.NewTip != ref.ID {
		t.Errorf("re-land = %+v, %v", again, err)
	}
	if mainTip(t, r).Version != ref.Version {
		t.Error("a no-op land moved main")
	}

	// A conflicting stack lands nothing.
	y := repotest.Commit(t, r, []hash.ID{root.ID}, "y", P("a.txt", "a by y\n"))
	res, err = r.Land(ctx, []hash.ID{y.ID}, repo.LandOptions{})
	if err != nil || res.Conflicting != y.ID || len(res.Conflicts) != 1 || res.Conflicts[0].Kind != repo.ConflictContent || len(res.Landed) != 0 {
		t.Errorf("conflicting land = %+v, %v", res, err)
	}
	if mainTip(t, r).Version != ref.Version {
		t.Error("a conflicting land moved main")
	}
	if _, err := r.Land(ctx, []hash.ID{y.ID}, repo.LandOptions{Target: "nope"}); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("land on a missing ref: %v", err)
	}
	verifyRepo(t, r)
}

// TestLandRetries moves main between an attempt's rebase and its CAS.
func TestLandRetries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, root := landEnv(t)
	r := env.Repo
	x := repotest.Commit(t, r, []hash.ID{root.ID}, "x", repotest.Put(t, r, "x.txt", "x\n"))
	n := 0
	race := func(moves int) func(context.Context, int) error {
		return func(ctx context.Context, attempt int) error {
			if attempt > moves {
				return nil
			}
			n++
			tip := mainTip(t, r)
			c := repotest.Commit(t, r, []hash.ID{tip.ID}, "concurrent", repotest.Put(t, r, "c.txt", fmt.Sprintf("%d\n", n)))
			_, err := r.UpdateRef(ctx, repo.MainRef, tip.ID, c.ID, false, "other", "land")
			return err
		}
	}
	res, err := r.Land(ctx, []hash.ID{x.ID}, repo.LandOptions{BeforeCAS: race(2)})
	if err != nil || res.Attempts != 3 || len(res.Landed) != 1 {
		t.Fatalf("land with 2 lost races = %+v, %v", res, err)
	}
	landed, err := r.Changeset(ctx, res.Landed[0])
	if err != nil || landed.Parents[0] == root.ID {
		t.Errorf("the landed changeset was not rebased onto the moved tip: %v", err)
	}

	y := repotest.Commit(t, r, []hash.ID{root.ID}, "y", repotest.Put(t, r, "y.txt", "y\n"))
	before := mainTip(t, r)
	_, err = r.Land(ctx, []hash.ID{y.ID}, repo.LandOptions{MaxAttempts: 3, BeforeCAS: race(100)})
	if !errors.Is(err, repo.ErrConflict) {
		t.Errorf("land that always loses the race: %v", err)
	}
	if got := mainTip(t, r); got.Version != before.Version+3 {
		t.Errorf("main version %d, want %d (only the racing updates)", got.Version, before.Version+3)
	}
	verifyRepo(t, r)
}
