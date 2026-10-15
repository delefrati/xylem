//go:build integration

package client_test

import (
	"context"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/server/servertest"
	"github.com/delefrati/xylem/internal/testutil"
)

func dial(t *testing.T, addr string) *client.Conn {
	t.Helper()
	c, err := client.Dial(addr, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func rootOf(t *testing.T, s *servertest.Server) hash.ID {
	t.Helper()
	cs, err := s.Env.Repo.Changeset(context.Background(), s.TipID)
	if err != nil {
		t.Fatal(err)
	}
	return cs.Core.RootTree
}

func TestFetchFullAndSparse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := servertest.Start(t, "branchy")
	conn := dial(t, s.Addr)
	root := rootOf(t, s)
	tip, _ := gitsource.ParseSHA(strings.TrimSpace(gitOut(t, s.GitDir, "rev-parse", "main")))
	gitFiles, err := s.Git.LsTree(ctx, tip)
	if err != nil {
		t.Fatal(err)
	}

	// Full fetch: every file, each object read once; a second fetch is free.
	cache := client.NewObjectCache(t.TempDir(), 0)
	f := client.NewFetcher(conn, "default", cache)
	files, err := f.Fetch(ctx, root, client.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(gitFiles) {
		t.Fatalf("full fetch: %d files, git %d", len(files), len(gitFiles))
	}
	stats := f.Stats()
	if int(stats.Blobs) != distinctBlobs(gitFiles) {
		t.Errorf("fetched %d blobs, want %d distinct", stats.Blobs, distinctBlobs(gitFiles))
	}
	again := client.NewFetcher(conn, "default", cache)
	if _, err := again.Fetch(ctx, root, client.NewScope(nil)); err != nil || again.Stats() != (client.FetchStats{}) {
		t.Errorf("cached fetch transferred %+v, %v", again.Stats(), err)
	}

	// Sparse fetch of one directory: only trees on the way to and under it,
	// plus the blobs of in-scope files (root files included).
	s.Store.Reset()
	scope := client.NewScope([][]byte{[]byte("internal/api")})
	sparseCache := client.NewObjectCache(t.TempDir(), 0)
	sf := client.NewFetcher(conn, "default", sparseCache)
	sparse, err := sf.Fetch(ctx, root, scope)
	if err != nil {
		t.Fatal(err)
	}
	wantBlobs := map[string]bool{}
	for _, e := range gitFiles {
		if strings.HasPrefix(string(e.Path), "internal/api/") || !strings.Contains(string(e.Path), "/") {
			wantBlobs[string(e.Path)] = true
		}
	}
	if len(sparse) != len(wantBlobs) {
		t.Errorf("sparse fetch: %d files, want %d", len(sparse), len(wantBlobs))
	}
	for _, fe := range sparse {
		if !wantBlobs[string(fe.Path)] {
			t.Errorf("out-of-scope file %q fetched", fe.Path)
		}
	}
	// Server side: trees read are exactly root, internal, internal/api (+ below).
	reads := s.Store.Reads()
	trees := int(sf.Stats().Trees)
	if len(reads) != trees+int(sf.Stats().Blobs) {
		t.Errorf("server read %d objects, client fetched %d trees + %d blobs", len(reads), trees, sf.Stats().Blobs)
	}
	if trees != 3 {
		t.Errorf("sparse fetch read %d trees, want 3 (root, internal, internal/api)", trees)
	}
}

func distinctBlobs(entries []gitsource.TreeEntry) int {
	seen := map[gitsource.SHA]bool{}
	for _, e := range entries {
		seen[e.SHA] = true
	}
	return len(seen)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return testutil.Git(t, dir, args...)
}
