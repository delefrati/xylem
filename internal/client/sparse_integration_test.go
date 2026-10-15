//go:build integration

package client_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/server/servertest"
)

func under(dirs ...string) func(string) bool {
	return func(p string) bool {
		if !strings.Contains(p, "/") {
			return true // root files are always in scope
		}
		for _, d := range dirs {
			if strings.HasPrefix(p, d+"/") {
				return true
			}
		}
		return false
	}
}

func TestSparseAddRemove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := servertest.Start(t, "branchy")
	tip := strings.TrimSpace(gitOut(t, s.GitDir, "rev-parse", "main"))
	dir := filepath.Join(t.TempDir(), "ws")
	res, err := client.Clone(ctx, s.Addr, dir, client.CloneOptions{Sparse: []string{"internal/api"}})
	if err != nil {
		t.Fatal(err)
	}
	w := res.Workspace
	f := client.NewFetcher(dial(t, s.Addr), "default", client.NewObjectCache(w.Meta("objects"), 0))
	assertMatchesGit(t, dir, s, tip, under("internal/api"))

	apply := func(change func(profile, dirs []client.Path) ([]client.Path, error), dirs ...string) error {
		ps := make([]client.Path, len(dirs))
		for i, d := range dirs {
			ps[i] = client.Path(d)
		}
		profile, err := change(w.State.Sparse, ps)
		if err != nil {
			return err
		}
		_, err = w.SetSparse(ctx, f, profile)
		return err
	}

	// Add: the new directories are fetched and written.
	s.Store.Reset()
	if err := apply(client.SparseAdd, "docs", "pkg/client"); err != nil {
		t.Fatal(err)
	}
	assertMatchesGit(t, dir, s, tip, under("internal/api", "docs", "pkg/client"))
	if len(s.Store.Reads()) == 0 {
		t.Error("sparse add fetched nothing")
	}
	if got := statusOf(t, w, f); len(got) != 0 {
		t.Errorf("status after sparse add %q", got)
	}

	// Remove: out-of-scope files are deleted and empty directories pruned.
	if err := apply(client.SparseRemove, "internal/api"); err != nil {
		t.Fatal(err)
	}
	assertMatchesGit(t, dir, s, tip, under("docs", "pkg/client"))
	if _, err := os.Stat(filepath.Join(dir, "internal")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("internal/ left behind: %v", err)
	}

	// A dirty file in a directory being removed refuses the change.
	docs, err := os.ReadDir(filepath.Join(dir, "docs"))
	if err != nil || len(docs) == 0 {
		t.Fatalf("docs: %v", err)
	}
	edited := filepath.Join(dir, "docs", docs[0].Name())
	if err := os.WriteFile(edited, []byte("local edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = apply(client.SparseRemove, "docs")
	var dirty *client.DirtyError
	if client.ExitCode(err) != client.ExitDirty || !errors.As(err, &dirty) {
		t.Fatalf("removing a dirty directory: %v", err)
	}
	reopened, _ := client.Open(dir)
	if len(reopened.State.Sparse) != 2 {
		t.Errorf("a refused change altered the profile: %q", reopened.State.Sparse)
	}
	if b, _ := os.ReadFile(edited); string(b) != "local edit\n" {
		t.Error("a refused change touched the dirty file")
	}
}
