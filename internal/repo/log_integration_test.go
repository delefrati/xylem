//go:build integration

package repo_test

import (
	"context"
	"slices"
	"testing"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
)

// logIDs pages through repo.Log with the given page size.
func logIDs(t *testing.T, r *repo.Repo, tip hash.ID, path string, page int) []hash.ID {
	t.Helper()
	ctx := context.Background()
	cur, err := r.StartLog(ctx, tip)
	if err != nil {
		t.Fatal(err)
	}
	var out []hash.ID
	for {
		ids, next, err := r.Log(ctx, cur, []byte(path), page)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ids...)
		if next == nil {
			return out
		}
		cur = *next
	}
}

func TestLogDraftsAndMainline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := repotest.New(t)
	// main: m0 ← m1 ← m2; drafts d1 ← d2 ← d3 on top of m2; side branch s1 off m0.
	m0 := repotest.Commit(t, r, nil, "m0", repotest.Put(t, r, "a/x", "0"), repotest.Put(t, r, "b", "0"))
	m1 := repotest.Commit(t, r, []hash.ID{m0.ID}, "m1", repotest.Put(t, r, "a/x", "1"))
	m2 := repotest.Commit(t, r, []hash.ID{m1.ID}, "m2", repotest.Put(t, r, "b", "2"))
	if _, err := r.UpdateRef(ctx, repo.MainRef, hash.ID{}, m2.ID, false, "t", "admin"); err != nil {
		t.Fatal(err)
	}
	d1 := repotest.Commit(t, r, []hash.ID{m2.ID}, "d1", repotest.Put(t, r, "a/y", "1"))
	d2 := repotest.Commit(t, r, []hash.ID{d1.ID}, "d2", repotest.Put(t, r, "b", "d2"))
	d3 := repotest.Commit(t, r, []hash.ID{d2.ID}, "d3", repotest.Put(t, r, "a/x", "d3"))
	s1 := repotest.Commit(t, r, []hash.ID{m0.ID}, "s1", repotest.Put(t, r, "a/x", "s1"))

	ids := func(cs ...*repo.Changeset) []hash.ID {
		var out []hash.ID
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	tests := []struct {
		tip  *repo.Changeset
		path string
		want []hash.ID
	}{
		{d3, "", ids(d3, d2, d1, m2, m1, m0)},
		{d3, "a", ids(d3, d1, m1, m0)},
		{d3, "a/x", ids(d3, m1, m0)},
		{d3, "b", ids(d2, m2, m0)},
		{d3, "nope", nil},
		{m2, "a", ids(m1, m0)},
		{s1, "a/x", ids(s1, m0)},
		{s1, "", ids(s1, m0)},
	}
	for _, tt := range tests {
		for _, page := range []int{1, 2, 100} {
			if got := logIDs(t, r, tt.tip.ID, tt.path, page); !slices.Equal(got, tt.want) {
				t.Errorf("log(%s, %q, page %d) = %v, want %v", tt.tip.Summary(), tt.path, page, short(got), short(tt.want))
			}
		}
	}
}

func TestLogWithoutMainline(t *testing.T) {
	t.Parallel()
	r := repotest.New(t) // no main ref: the whole history is walked one by one
	var tip *repo.Changeset
	var parents []hash.ID
	var all []hash.ID
	for i := range 12 {
		files := repotest.Put(t, r, "f", string(rune('a'+i)))
		tip = repotest.Commit(t, r, parents, "c", files)
		parents = []hash.ID{tip.ID}
		all = append([]hash.ID{tip.ID}, all...)
	}
	if got := logIDs(t, r, tip.ID, "", 5); !slices.Equal(got, all) {
		t.Errorf("log without mainline = %d entries, want %d", len(got), len(all))
	}
	if got := logIDs(t, r, tip.ID, "f", 4); !slices.Equal(got, all) {
		t.Errorf("path log without mainline = %d entries", len(got))
	}
}

func short(ids []hash.ID) []string {
	var out []string
	for _, id := range ids {
		out = append(out, id.Short(6))
	}
	return out
}
