//go:build parity

package parity_test

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/importer"
	"github.com/delefrati/xylem/internal/parity"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/testutil"
)

// TestParity imports each fixture into a fresh database, serves it in
// process and runs every parity check (M4 acceptance: 100% on tiny and edge;
// on branchy 100% except blame ≥ 99%).
func TestParity(t *testing.T) {
	cases := []struct {
		profile        string
		commits        int
		blameThreshold float64
	}{
		{"tiny", 0, 100},
		{"edge", 0, 100},
		{"branchy", 150, 99},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			t.Parallel()
			runParity(t, tc.profile, testutil.GitFixture(t, tc.profile), parity.Options{Commits: tc.commits, BlameThreshold: tc.blameThreshold})
		})
	}
}

// TestParityExternal runs the suite on the real repositories fetched by
// scripts/fetch-external-fixtures.sh into testdata/external (make
// parity-external). Missing repositories are skipped.
func TestParityExternal(t *testing.T) {
	for _, name := range []string{"cobra", "prometheus"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(testutil.RepoRoot(t), "testdata", "external", name)
			if _, err := os.Stat(dir); err != nil {
				t.Skipf("%s not fetched (scripts/fetch-external-fixtures.sh): %v", name, err)
			}
			runParity(t, name, dir, parity.Options{Commits: 100, BlameThreshold: 99})
		})
	}
}

// runParity imports the git repository at dir into a fresh database (objects
// in a temporary FS store), serves it in process and runs every check.
func runParity(t *testing.T, label, dir string, opts parity.Options) {
	t.Helper()
	ctx := context.Background()
	src, err := gitsource.Open(ctx, dir, gitsource.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	env := repotest.NewEnv(t, store.NewFS(t.TempDir()))
	im, err := importer.New(env.Repo, src, importer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	stats, err := im.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: imported in %v: %+v", label, time.Since(start).Round(time.Millisecond), stats)

	srv := server.New(server.Config{Meta: env.Meta, Objects: env.Objects, ObjectCacheBytes: 256 << 20})
	g := srv.NewGRPCServer()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(lis) }()
	defer g.Stop()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(server.MaxMessageSize)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	start = time.Now()
	report, err := parity.Run(ctx, src, parity.Clients{Repo: xylemv1.NewRepoServiceClient(conn), Object: xylemv1.NewObjectServiceClient(conn), Addr: lis.Addr().String()}, opts)
	if err != nil {
		t.Fatal(err)
	}
	var table bytes.Buffer
	if err := report.Print(&table); err != nil {
		t.Fatal(err)
	}
	t.Logf("parity %s (%v):\n%s", label, time.Since(start).Round(time.Millisecond), table.String())
	if os.Getenv("PARITY_REPORT") != "" {
		if err := report.WriteJSON(filepath.Join(testutil.RepoRoot(t), "parity-report-"+label+".json")); err != nil {
			t.Fatal(err)
		}
	}
	if !report.Pass() {
		t.Errorf("parity failed on %s", label)
	}
}
