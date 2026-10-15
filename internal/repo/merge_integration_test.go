//go:build integration

package repo_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/store"
)

const tenLines = "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"

func content(t *testing.T, r *repo.Repo, cs *repo.Changeset, path string) (string, object.Mode) {
	t.Helper()
	e, ok := lookup(t, r, cs.Core.RootTree, path)
	if !ok {
		t.Fatalf("%s missing", path)
	}
	stored, err := r.Objects().Get(context.Background(), e.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, body, err := object.Decode(stored)
	if err != nil {
		t.Fatal(err)
	}
	return string(body), e.Mode
}

func withMode(ch changeset.Change, m object.Mode) changeset.Change { ch.Mode = m; return ch }

func TestRebaseLineMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := repotest.NewEnv(t, store.NewMem())
	var clean, conflicted atomic.Int64
	r, err := repo.Open(ctx, env.Meta, env.Objects, "default", repo.Options{Now: repotest.Clock, OnMerge: func(ok bool) {
		if ok {
			clean.Add(1)
		} else {
			conflicted.Add(1)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	P := func(path, content string) changeset.Change { return repotest.Put(t, r, path, content) }
	root := repotest.Commit(t, r, nil, "root", P("f.txt", tenLines), P("g.txt", tenLines), P("link", "target"), P("bin", "a\x00b\n"))
	onto := repotest.Commit(t, r, []hash.ID{root.ID}, "onto",
		P("f.txt", "one\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"), // line 1
		withMode(P("g.txt", "one\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"), object.ModeExec),
		P("link", "other"), P("bin", "a\x00c\n"), P("new.txt", "ours\n"))
	stack := func(msg string, changes ...changeset.Change) []hash.ID {
		return []hash.ID{repotest.Commit(t, r, []hash.ID{root.ID}, msg, changes...).ID}
	}
	opts := repo.RebaseOptions{Time: 1_800_000_000}

	// Different regions of the same file: merged.
	res, err := r.RebaseStack(ctx, stack("x edits line 10", P("f.txt", "1\n2\n3\n4\n5\n6\n7\n8\n9\nten\n")), onto.ID, opts)
	if err != nil || len(res.Conflicts) != 0 || len(res.Rebased) != 1 {
		t.Fatalf("disjoint regions = %+v, %v", res, err)
	}
	if got, _ := content(t, r, get(t, r, res.Tip), "f.txt"); got != "one\n2\n3\n4\n5\n6\n7\n8\n9\nten\n" {
		t.Errorf("merged f.txt = %q", got)
	}
	// Mode from onto, content from both.
	res, err = r.RebaseStack(ctx, stack("x edits g line 10", P("g.txt", "1\n2\n3\n4\n5\n6\n7\n8\n9\nten\n")), onto.ID, opts)
	if err != nil || len(res.Conflicts) != 0 {
		t.Fatalf("mode + content = %+v, %v", res, err)
	}
	if got, mode := content(t, r, get(t, r, res.Tip), "g.txt"); got != "one\n2\n3\n4\n5\n6\n7\n8\n9\nten\n" || mode != object.ModeExec {
		t.Errorf("merged g.txt = %q (%v)", got, mode)
	}
	if clean.Load() != 2 {
		t.Errorf("OnMerge clean = %d, want 2", clean.Load())
	}

	for _, tc := range []struct {
		name    string
		change  changeset.Change
		kind    repo.ConflictKind
		regions int
	}{
		{"same region", P("f.txt", "uno\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"), repo.ConflictContent, 1},
		{"symlink targets", withMode(P("link", "mine"), object.ModeSymlink), repo.ConflictContent, 0},
		{"binary", P("bin", "a\x00d\n"), repo.ConflictContent, 0},
		{"add/add, empty base", P("new.txt", "theirs\n"), repo.ConflictAddAdd, 1},
	} {
		res, err := r.RebaseStack(ctx, stack(tc.name, tc.change), onto.ID, opts)
		if err != nil || len(res.Conflicts) != 1 {
			t.Errorf("%s: %+v, %v", tc.name, res, err)
			continue
		}
		if c := res.Conflicts[0]; c.Kind != tc.kind || c.Regions != tc.regions {
			t.Errorf("%s: conflict %v with %d regions, want %v with %d", tc.name, c.Kind, c.Regions, tc.kind, tc.regions)
		}
	}
	if conflicted.Load() != 2 {
		t.Errorf("OnMerge conflicts = %d, want 2 (same region, add/add)", conflicted.Load())
	}

	// Above MergeMaxBytes, content conflicts stay file-level.
	small, err := repo.Open(ctx, env.Meta, env.Objects, "default", repo.Options{Now: repotest.Clock, MergeMaxBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	res, err = small.RebaseStack(ctx, stack("too big", P("f.txt", "1\n2\n3\n4\n5\n6\n7\n8\n9\nTEN\n")), onto.ID, opts)
	if err != nil || len(res.Conflicts) != 1 || res.Conflicts[0].Kind != repo.ConflictContent || res.Conflicts[0].Regions != 0 {
		t.Errorf("over the merge limit = %+v, %v", res, err)
	}
	verifyRepo(t, r)
}
