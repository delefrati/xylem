//go:build integration

package repo_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
)

// content returns the blob id at path in a changeset's tree, or the zero id.
func blobAt(t *testing.T, r *repo.Repo, cs *repo.Changeset, path string) hash.ID {
	t.Helper()
	e, ok := lookup(t, r, cs.Core.RootTree, path)
	if !ok {
		return hash.ID{}
	}
	return e.ID
}

func get(t *testing.T, r *repo.Repo, id hash.ID) *repo.Changeset {
	t.Helper()
	cs, err := r.Changeset(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestRebaseStack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := repotest.New(t)
	P := func(path, content string) changeset.Change { return repotest.Put(t, r, path, content) }
	root := repotest.Commit(t, r, nil, "root", P("README", "readme\n"), P("a.txt", "a\n"), P("d/x", "x\n"))
	x1 := repotest.Commit(t, r, []hash.ID{root.ID}, "x1", P("a.txt", "a by x1\n"))
	x2 := repotest.Commit(t, r, []hash.ID{x1.ID}, "x2", P("b.txt", "b\n"))
	t1 := repotest.Commit(t, r, []hash.ID{root.ID}, "t1", P("README", "readme on main\n"))
	stack := []hash.ID{x1.ID, x2.ID}
	opts := repo.RebaseOptions{Time: 1_800_000_000}

	t.Run("rebase", func(t *testing.T) {
		res, err := r.RebaseStack(ctx, stack, t1.ID, opts)
		if err != nil || len(res.Conflicts) != 0 || len(res.Rebased) != 2 || len(res.Skipped) != 0 || res.Tip != res.Rebased[1] {
			t.Fatalf("rebase = %+v, %v", res, err)
		}
		n1, n2 := get(t, r, res.Rebased[0]), get(t, r, res.Rebased[1])
		if n1.Parents[0] != t1.ID || n2.Parents[0] != n1.ID {
			t.Errorf("parents %v %v", n1.Parents, n2.Parents)
		}
		if string(n1.Extras[changeset.ExtraRebasedFrom]) != x1.ID.String() || string(n2.Extras[changeset.ExtraRebasedFrom]) != x2.ID.String() {
			t.Errorf("rebased-from %q %q", n1.Extras, n2.Extras)
		}
		if n1.Committer.Time != opts.Time || string(n1.Committer.NameEmail) != string(x1.Author.NameEmail) ||
			!cmp.Equal(n1.Author, x1.Author) || string(n1.Message) != "x1" {
			t.Errorf("signatures/message %+v %+v %q", n1.Author, n1.Committer, n1.Message)
		}
		// The top has main's README, x1's a.txt and x2's b.txt.
		for path, want := range map[string]*repo.Changeset{"README": t1, "a.txt": x1, "b.txt": x2, "d/x": root} {
			if blobAt(t, r, n2, path) != blobAt(t, r, want, path) {
				t.Errorf("%s at the rebased top differs from %s", path, want.Message)
			}
		}
		// Deterministic: the same attempt time yields the same ids.
		again, err := r.RebaseStack(ctx, stack, t1.ID, opts)
		if err != nil || again.Tip != res.Tip {
			t.Errorf("repeat rebase tip %v vs %v (%v)", again.Tip, res.Tip, err)
		}
	})

	t.Run("already based", func(t *testing.T) {
		res, err := r.RebaseStack(ctx, stack, root.ID, opts)
		if err != nil || res.Rebased[0] != x1.ID || res.Rebased[1] != x2.ID || res.Tip != x2.ID {
			t.Fatalf("rebase onto its own base = %+v, %v", res, err)
		}
	})

	t.Run("skip already applied", func(t *testing.T) {
		t2 := repotest.Commit(t, r, []hash.ID{t1.ID}, "t2 has x1's change", P("a.txt", "a by x1\n"))
		res, err := r.RebaseStack(ctx, stack, t2.ID, opts)
		if err != nil || len(res.Skipped) != 1 || res.Skipped[0] != x1.ID || len(res.Rebased) != 1 {
			t.Fatalf("rebase = %+v, %v", res, err)
		}
		if n2 := get(t, r, res.Rebased[0]); n2.Parents[0] != t2.ID {
			t.Errorf("x2 rebased onto %v, want t2", n2.Parents[0])
		}
		t3 := repotest.Commit(t, r, []hash.ID{t2.ID}, "t3 has x2's change", P("b.txt", "b\n"))
		res, err = r.RebaseStack(ctx, stack, t3.ID, opts)
		if err != nil || len(res.Skipped) != 2 || len(res.Rebased) != 0 || res.Tip != t3.ID {
			t.Errorf("all skipped = %+v, %v", res, err)
		}
	})

	t.Run("content conflict", func(t *testing.T) {
		c := repotest.Commit(t, r, []hash.ID{t1.ID}, "main edits a.txt", P("a.txt", "a by main\n"))
		res, err := r.RebaseStack(ctx, stack, c.ID, opts)
		if err != nil || res.Conflicting != x1.ID || len(res.Rebased) != 0 || len(res.Conflicts) != 1 {
			t.Fatalf("rebase = %+v, %v", res, err)
		}
		got := res.Conflicts[0]
		if string(got.Path) != "a.txt" || got.Kind != repo.ConflictContent ||
			got.Base != blobAt(t, r, root, "a.txt") || got.Ours != blobAt(t, r, c, "a.txt") || got.Theirs != blobAt(t, r, x1, "a.txt") {
			t.Errorf("conflict %+v", got)
		}
	})

	t.Run("structural file/directory", func(t *testing.T) {
		y := repotest.Commit(t, r, []hash.ID{root.ID}, "y adds e/f", P("e/f", "f\n"))
		e := repotest.Commit(t, r, []hash.ID{t1.ID}, "main adds file e", P("e", "e\n"))
		res, err := r.RebaseStack(ctx, []hash.ID{y.ID}, e.ID, opts)
		if err != nil || res.Conflicting != y.ID || len(res.Conflicts) != 1 || res.Conflicts[0].Kind != repo.ConflictFileDirectory {
			t.Fatalf("rebase = %+v, %v", res, err)
		}
	})

	t.Run("git-sha dropped", func(t *testing.T) {
		g, _, err := r.CreateChangeset(ctx, repo.CreateRequest{
			Parents: []hash.ID{root.ID}, Author: repotest.Author, Message: []byte("imported"),
			Extras:  map[string][]byte{changeset.ExtraGitSHA: []byte("0123456789abcdef0123456789abcdef01234567"), "note": []byte("kept")},
			Changes: []changeset.Change{P("g.txt", "g\n")},
		}, repo.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.RebaseStack(ctx, []hash.ID{g.ID}, t1.ID, opts)
		if err != nil || len(res.Rebased) != 1 {
			t.Fatalf("rebase = %+v, %v", res, err)
		}
		n := get(t, r, res.Rebased[0])
		if _, ok := n.Extras[changeset.ExtraGitSHA]; ok || string(n.Extras["note"]) != "kept" {
			t.Errorf("extras %q", n.Extras)
		}
	})

	t.Run("malformed stacks", func(t *testing.T) {
		merge := repotest.Commit(t, r, []hash.ID{x1.ID, t1.ID}, "merge")
		for name, tc := range map[string]struct {
			stack []hash.ID
			onto  hash.ID
			want  error
		}{
			"empty":        {nil, t1.ID, repo.ErrInvalid},
			"not a chain":  {[]hash.ID{x2.ID, x1.ID}, t1.ID, repo.ErrInvalid},
			"gap":          {[]hash.ID{x1.ID, t1.ID}, t1.ID, repo.ErrInvalid},
			"merge":        {[]hash.ID{merge.ID}, t1.ID, repo.ErrInvalid},
			"root":         {[]hash.ID{root.ID}, t1.ID, repo.ErrInvalid},
			"unknown":      {[]hash.ID{{1}}, t1.ID, repo.ErrNotFound},
			"unknown onto": {stack, hash.ID{2}, repo.ErrNotFound},
		} {
			if _, err := r.RebaseStack(ctx, tc.stack, tc.onto, opts); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", name, err, tc.want)
			}
		}
	})
}
