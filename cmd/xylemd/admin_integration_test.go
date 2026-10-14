//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/testutil"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append(args, "--log-level", "warn"))
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestAdminInitRepoAndSetRef(t *testing.T) {
	pool := testutil.NewDB(t)
	dbURL := pool.Config().ConnString()
	storeDir := t.TempDir()
	flags := []string{"--db", dbURL, "--store", "fs://" + storeDir}

	out, err := run(t, append([]string{"admin", "init-repo", "proj"}, flags...)...)
	if err != nil || !strings.Contains(out, "created repo proj; main -> ") {
		t.Fatalf("init-repo: %q, %v", out, err)
	}
	if _, err := run(t, append([]string{"admin", "init-repo", "proj"}, flags...)...); err == nil {
		t.Error("second init-repo succeeded")
	}

	ctx := context.Background()
	r, err := repo.Open(ctx, meta.NewPG(pool), store.NewFS(storeDir), "proj", repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	main, err := r.GetRef(ctx, repo.MainRef)
	if err != nil {
		t.Fatal(err)
	}
	a := repotest.Commit(t, r, []hash.ID{main.ID}, "a", repotest.Put(t, r, "f", "a"))
	b := repotest.Commit(t, r, []hash.ID{a.ID}, "b", repotest.Put(t, r, "f", "b"))

	// Fast-forward by unique prefix.
	out, err = run(t, append([]string{"admin", "set-ref", "proj", "main", b.ID.Short(10)}, flags...)...)
	if err != nil || !strings.Contains(out, "main -> "+b.ID.String()+" (version 2)") {
		t.Fatalf("set-ref fast-forward: %q, %v", out, err)
	}
	// Going back needs --force.
	if _, err := run(t, append([]string{"admin", "set-ref", "proj", "main", a.ID.String()}, flags...)...); err == nil {
		t.Error("non-fast-forward set-ref without --force succeeded")
	}
	if out, err := run(t, append([]string{"admin", "set-ref", "proj", "main", a.ID.String(), "--force"}, flags...)...); err != nil {
		t.Errorf("forced set-ref: %q, %v", out, err)
	}
	// A stale --expected loses the compare-and-swap.
	if _, err := run(t, append([]string{"admin", "set-ref", "proj", "main", b.ID.String(), "--expected", b.ID.String()}, flags...)...); err == nil {
		t.Error("set-ref with stale --expected succeeded")
	}
	// A new ref is created.
	if out, err := run(t, append([]string{"admin", "set-ref", "proj", "release/1", a.ID.String()}, flags...)...); err != nil || !strings.Contains(out, "(version 1)") {
		t.Errorf("create release/1: %q, %v", out, err)
	}
	if _, err := run(t, append([]string{"admin", "set-ref", "nope", "main", a.ID.String()}, flags...)...); err == nil {
		t.Error("set-ref on unknown repo succeeded")
	}

	// verify passes on this history, then fails once a row is tampered with.
	out, err = run(t, append([]string{"verify", "proj", "--all", "--objects"}, flags...)...)
	if err != nil || !strings.Contains(out, "checked 3 changesets and 2 refs: 0 violations") {
		t.Fatalf("verify: %q, %v", out, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE core_derived SET gen = 99 WHERE changeset_id = $1", b.ID[:]); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, append([]string{"verify", "proj"}, flags...)...)
	if err == nil || !strings.Contains(out, "VIOLATION") || !strings.Contains(out, "1 violations") {
		t.Errorf("verify after tampering: %q, %v", out, err)
	}
	if _, err := run(t, append([]string{"verify", "proj", "--all", "--sample", "2"}, flags...)...); err == nil {
		t.Error("--all with --sample accepted")
	}
}
