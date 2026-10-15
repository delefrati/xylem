//go:build integration

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/server/servertest"
	"github.com/delefrati/xylem/internal/testutil"
)

func TestCheckoutAndSparseCommands(t *testing.T) {
	t.Parallel()
	s := servertest.Start(t, "tiny")
	git := func(args ...string) string { return testutil.GitRaw(t, s.GitDir, args...) }
	dir := filepath.Join(t.TempDir(), "ws")
	runOK(t, "clone", s.Addr, dir)
	readme := filepath.Join(dir, "README")
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	out := runOK(t, "-C", dir, "checkout", "main~2")
	if !strings.HasPrefix(out, "at ") || read(readme) != git("show", "main~2:README") {
		t.Errorf("checkout main~2: %q, README %q", out, read(readme))
	}
	if err := os.WriteFile(readme, []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runXY("-C", dir, "checkout", "main"); client.ExitCode(err) != client.ExitDirty {
		t.Errorf("dirty checkout: %v", err)
	}
	// checkout --force @ discards the local change.
	runOK(t, "-C", dir, "checkout", "--force", "@")
	if read(readme) != git("show", "main~2:README") {
		t.Errorf("README after checkout -f @: %q", read(readme))
	}
	runOK(t, "-C", dir, "checkout", "main")
	if out := runOK(t, "-C", dir, "status"); out != "" || read(readme) != git("show", "main:README") {
		t.Errorf("after checkout main: status %q", out)
	}

	// sparse on a full checkout.
	if out := runOK(t, "-C", dir, "sparse", "list"); out != "" {
		t.Errorf("sparse list on a full checkout: %q", out)
	}
	if _, err := runXY("-C", dir, "sparse", "add", "docs"); client.ExitCode(err) != client.ExitUsage {
		t.Errorf("sparse add on a full checkout: %v", err)
	}

	sparse := filepath.Join(t.TempDir(), "sparse")
	runOK(t, "clone", s.Addr, sparse, "--sparse", "docs")
	if out := runOK(t, "-C", sparse, "sparse", "list"); out != "docs\n" {
		t.Errorf("sparse list: %q", out)
	}
	runOK(t, "-C", sparse, "sparse", "add", "src")
	if read(filepath.Join(sparse, "src", "main.go")) != git("show", "main:src/main.go") {
		t.Error("sparse add src did not check out src/main.go")
	}
	out = runOK(t, "-C", filepath.Join(sparse, "src"), "sparse", "remove", "../docs", "--json")
	if !strings.Contains(out, `"sparse": [`) || !strings.Contains(out, `"src"`) || !strings.Contains(out, `"deleted": 1`) {
		t.Errorf("sparse remove --json: %s", out)
	}
	if _, err := os.Stat(filepath.Join(sparse, "docs")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("docs/ after sparse remove: %v", err)
	}
	if _, err := runXY("-C", sparse, "sparse", "remove", "src"); client.ExitCode(err) != client.ExitUsage {
		t.Errorf("removing the last directory: %v", err)
	}
	if _, err := runXY("-C", sparse, "sparse", "add"); client.ExitCode(err) != client.ExitUsage {
		t.Errorf("sparse add without a directory: %v", err)
	}
}
