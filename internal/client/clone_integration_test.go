//go:build integration

package client_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/server/servertest"
)

func TestClone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := servertest.Start(t, "branchy")
	tip := strings.TrimSpace(gitOut(t, s.GitDir, "rev-parse", "main"))

	full := filepath.Join(t.TempDir(), "full")
	res, err := client.Clone(ctx, s.Addr, full, client.CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changeset != s.TipID || res.Files == 0 {
		t.Errorf("clone result %+v", res)
	}
	assertMatchesGit(t, full, s, tip, nil)

	sparse := filepath.Join(t.TempDir(), "sparse")
	if _, err := client.Clone(ctx, s.Addr, sparse, client.CloneOptions{Sparse: []string{"internal/api/", "docs"}}); err != nil {
		t.Fatal(err)
	}
	assertMatchesGit(t, sparse, s, tip, func(p string) bool {
		return !strings.Contains(p, "/") || strings.HasPrefix(p, "internal/api/") || strings.HasPrefix(p, "docs/")
	})
	w, _ := client.Open(sparse)
	if len(w.State.Sparse) != 2 || string(w.State.Sparse[0]) != "internal/api" {
		t.Errorf("sparse profile %q", w.State.Sparse)
	}

	// An older revision.
	old := filepath.Join(t.TempDir(), "old")
	oldSHA := strings.TrimSpace(gitOut(t, s.GitDir, "rev-parse", "main~40"))
	if _, err := client.Clone(ctx, s.Addr, old, client.CloneOptions{Rev: "git:" + oldSHA}); err != nil {
		t.Fatal(err)
	}
	assertMatchesGit(t, old, s, oldSHA, nil)
}

func TestCloneErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := servertest.Start(t, "tiny")
	busy := t.TempDir()
	if err := os.WriteFile(filepath.Join(busy, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Clone(ctx, s.Addr, busy, client.CloneOptions{}); client.ExitCode(err) != client.ExitUsage {
		t.Errorf("non-empty destination: %v", err)
	}
	empty := t.TempDir() // an existing empty directory is fine
	if _, err := client.Clone(ctx, s.Addr, empty, client.CloneOptions{}); err != nil {
		t.Errorf("empty destination: %v", err)
	}
	gone := filepath.Join(t.TempDir(), "gone")
	if _, err := client.Clone(ctx, s.Addr, gone, client.CloneOptions{Rev: "nope"}); err == nil {
		t.Error("clone of an unknown revision succeeded")
	}
	if _, err := os.Stat(gone); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a failed clone left its directory behind")
	}
	if _, err := client.Clone(ctx, s.Addr, filepath.Join(t.TempDir(), "x"), client.CloneOptions{Sparse: []string{"/abs"}}); client.ExitCode(err) != client.ExitUsage {
		t.Errorf("bad sparse dir: %v", err)
	}
}
