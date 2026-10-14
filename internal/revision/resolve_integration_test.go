//go:build integration

package revision_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/revision"
)

func TestResolve(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := repotest.New(t)
	commit := func(parents []hash.ID, msg, gitSHA string) *repo.Changeset {
		t.Helper()
		var extras map[string][]byte
		if gitSHA != "" {
			extras = map[string][]byte{changeset.ExtraGitSHA: []byte(gitSHA)}
		}
		cs, _, err := r.CreateChangeset(ctx, repo.CreateRequest{
			Parents: parents, Author: repotest.Author, Message: []byte(msg),
			Extras: extras, Changes: []changeset.Change{repotest.Put(t, r, "f", msg)},
		}, repo.CreateOptions{AllowRoot: true})
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}
	// c0 ← c1 ← c2 ← merge(c2, side) ← c4, side off c1.
	c0 := commit(nil, "c0", "aaaa1111"+strings.Repeat("0", 32))
	c1 := commit([]hash.ID{c0.ID}, "c1", "aaaa2222"+strings.Repeat("0", 32))
	c2 := commit([]hash.ID{c1.ID}, "c2", "")
	side := commit([]hash.ID{c1.ID}, "side", "")
	merge := commit([]hash.ID{c2.ID, side.ID}, "merge", "")
	c4 := commit([]hash.ID{merge.ID}, "c4", "")
	if _, err := r.UpdateRef(ctx, repo.MainRef, hash.ID{}, c4.ID, false, "t", "admin"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		rev  string
		want hash.ID
	}{
		{"main", c4.ID},
		{"main~0", c4.ID},
		{"main~1", merge.ID},
		{"main~", merge.ID},
		{"main^", merge.ID},
		{"main~2", c2.ID},
		{"main~4", c0.ID},
		{"main~1^2", side.ID},
		{"main~1^2~1", c1.ID},
		{"main^^2", side.ID},
		{"main~1^0", merge.ID},
		{c2.ID.String(), c2.ID},
		{c2.ID.Short(10), c2.ID},
		{strings.ToUpper(c2.ID.Short(12)) + "~1", c1.ID},
		{"git:aaaa1", c0.ID},
		{"git:aaaa2222" + strings.Repeat("0", 32), c1.ID},
		{"git:AAAA2~1", c0.ID},
	}
	for _, tt := range tests {
		got, err := revision.Resolve(ctx, r, tt.rev)
		if err != nil || got != tt.want {
			t.Errorf("Resolve(%q) = %s, %v; want %s", tt.rev, got.Short(8), err, tt.want.Short(8))
		}
	}

	errs := []struct {
		rev  string
		want error
		msg  string
	}{
		{"nope", repo.ErrNotFound, "unknown revision"},
		{"main~5", repo.ErrNotFound, "past the root"},
		{"main^2", repo.ErrNotFound, "has 1 parents"},
		{"main~1^3", repo.ErrNotFound, "has 2 parents"},
		{"git:aaaa", repo.ErrInvalid, "ambiguous"},
		{"git:bbbb", repo.ErrNotFound, "no changeset"},
		{"ffffffffff", repo.ErrNotFound, "no changeset"},
		{"@", repo.ErrInvalid, "workspace"},
		{"Main", repo.ErrInvalid, "invalid revision"},
	}
	for _, tt := range errs {
		_, err := revision.Resolve(ctx, r, tt.rev)
		if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("Resolve(%q) = %v; want %v containing %q", tt.rev, err, tt.want, tt.msg)
		}
	}
	// The ambiguity error lists the candidates.
	if _, err := revision.Resolve(ctx, r, "git:aaaa"); !strings.Contains(err.Error(), "git:aaaa1111") || !strings.Contains(err.Error(), "git:aaaa2222") {
		t.Errorf("ambiguous git prefix error does not list candidates: %v", err)
	}

	// A ref named like a hex prefix wins over the prefix.
	hexName := c2.ID.Short(8)
	if _, err := r.UpdateRef(ctx, hexName, hash.ID{}, c0.ID, false, "t", "admin"); err != nil {
		t.Fatal(err)
	}
	if got, err := revision.Resolve(ctx, r, hexName); err != nil || got != c0.ID {
		t.Errorf("ref %q resolved to %s, %v; want the ref's target %s", hexName, got.Short(8), err, c0.ID.Short(8))
	}
}
