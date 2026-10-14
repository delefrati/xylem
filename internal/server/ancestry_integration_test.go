//go:build integration

package server_test

import (
	"context"
	"encoding/hex"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/testutil"
)

func TestAncestryRPCsMatchGit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := startServer(t)
	importFixture(t, c, "branchy")
	dir := testutil.GitFixture(t, "branchy")
	shaMap, err := c.Env.Repo.GitSHAMap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[hash.ID]string{}
	for sha, id := range shaMap {
		byID[id] = hex.EncodeToString(sha[:])
	}
	all := strings.Split(testutil.Git(t, dir, "rev-list", "main"), "\n")
	r := rand.NewChaCha8([32]byte{5})
	pick := func() string { return all[int(r.Uint64()>>34)%len(all)] }
	multi := 0
	for range 60 {
		a, b := pick(), pick()
		resp, err := c.Repo.MergeBase(ctx, &xylemv1.MergeBaseRequest{A: "git:" + a, B: "git:" + b})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, id := range resp.Ids {
			var h hash.ID
			copy(h[:], id)
			got = append(got, byID[h])
		}
		slices.Sort(got)
		want := strings.Fields(testutil.Git(t, dir, "merge-base", "--all", a, b))
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("MergeBase(%s, %s) = %v, git %v", a[:8], b[:8], got, want)
		}
		if len(want) > 1 {
			multi++
		}
		for _, pair := range [][2]string{{a, b}, {b, a}} {
			isAnc, err := c.Repo.IsAncestor(ctx, &xylemv1.IsAncestorRequest{Ancestor: "git:" + pair[0], Descendant: "git:" + pair[1]})
			if err != nil {
				t.Fatal(err)
			}
			if want := gitIsAncestor(t, dir, pair[0], pair[1]); isAnc.Result != want {
				t.Errorf("IsAncestor(%s, %s) = %v, git %v", pair[0][:8], pair[1][:8], isAnc.Result, want)
			}
		}
	}
	t.Logf("%d sampled pairs had several merge bases", multi)

	// Errors and the cost limit.
	if _, err := c.Repo.MergeBase(ctx, &xylemv1.MergeBaseRequest{A: "main", B: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown revision: %v", err)
	}
	limited := startServerFor(t, repotest.NewEnv(t, store.NewMem()), func(cfg *server.Config) { cfg.MaxAncestryNodes = 5 })
	importFixture(t, limited, "branchy")
	if _, err := limited.Repo.MergeBase(ctx, &xylemv1.MergeBaseRequest{A: "main", B: "git:" + all[len(all)-1]}); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("over the node limit: %v", err)
	}
}

func gitIsAncestor(t *testing.T, dir, a, b string) bool {
	t.Helper()
	return testutil.Git(t, dir, "rev-list", "--count", a, "^"+b) == "0"
}
