//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/parity"
	"github.com/delefrati/xylem/internal/server/servertest"
)

// startChain runs server → xylemd cache → xylemd daemon and returns the
// daemon's address (unix://…).
func startChain(t *testing.T, s *servertest.Server) (daemon, cacheAddr string) {
	t.Helper()
	cacheAddr, cacheHTTP := freeAddr(t), freeAddr(t)
	startCmd(t, cacheHTTP, "cache", "--upstream", s.Addr, "--listen", cacheAddr, "--http-listen", cacheHTTP)
	sock := filepath.Join(t.TempDir(), "d.sock")
	daemonHTTP := freeAddr(t)
	startCmd(t, daemonHTTP, "daemon", "--upstream", cacheAddr, "--server", s.Addr, "--socket", sock,
		"--disk-dir", t.TempDir(), "--http-listen", daemonHTTP)
	return "unix://" + sock, cacheAddr
}

func TestDaemonChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := servertest.Start(t, "branchy")
	daemon, cacheAddr := startChain(t, s)

	// Strict tiering: a daemon needs a cache upstream (unless told otherwise).
	sock := filepath.Join(t.TempDir(), "x.sock")
	_, err := run(t, "daemon", "--upstream", s.Addr, "--server", s.Addr, "--socket", sock, "--disk-dir", t.TempDir(), "--http-listen", freeAddr(t))
	if err == nil || !strings.Contains(err.Error(), "upstream is a server, want a cache") {
		t.Errorf("daemon → server without --allow-direct-upstream: %v", err)
	}
	if _, err := run(t, "daemon", "--upstream", cacheAddr, "--socket", sock); err == nil || !strings.Contains(err.Error(), "--server is required") {
		t.Errorf("daemon without --server: %v", err)
	}

	// Clone parity (full and three sparse profiles) through daemon → cache → server.
	src, err := gitsource.Open(ctx, s.GitDir, gitsource.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	conn, err := client.Dial(daemon)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	report, err := parity.Run(ctx, src, parity.Clients{Repo: conn.Repo, Object: conn.Object, Addr: daemon},
		parity.Options{Checks: []string{"clone", "log", "read-file"}, Commits: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pass() {
		var b strings.Builder
		_ = report.Print(&b)
		t.Errorf("parity through the daemon:\n%s", b.String())
	}

	// A no-cache workspace behind the daemon: commit and land go through the
	// proxy (and PutObjects through both tiers), and nothing stays in
	// .xylem/objects.
	dir := filepath.Join(t.TempDir(), "ws")
	res, err := client.Clone(ctx, daemon, dir, client.CloneOptions{Sparse: []string{"docs"}, NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	w := res.Workspace
	if err := os.WriteFile(filepath.Join(dir, "docs", "via-daemon.txt"), []byte("hello through the daemon\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := client.NewFetcher(conn, "default", w.Cache())
	if _, err := w.Commit(ctx, f, client.CommitOptions{Message: []byte("via daemon\n"), All: true, Author: "Ada <ada@example.com>"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Land(ctx, f); err != nil {
		t.Fatal(err)
	}
	tip, err := conn.Repo.ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Revision: "main"})
	if err != nil || string(tip.GetId()) != string(w.State.Parent[:]) {
		t.Errorf("main after landing through the daemon: %v", err)
	}
	stats, err := w.Cache().Stats()
	if err != nil || stats.Objects != 0 {
		t.Errorf("no-cache workspace holds %d objects (%v)", stats.Objects, err)
	}
}
