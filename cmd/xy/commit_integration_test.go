//go:build integration

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/server/servertest"
)

// Not parallel: the author comes from XYLEM_AUTHOR.
func TestAddAndCommitCommands(t *testing.T) {
	t.Setenv("XYLEM_AUTHOR", "Ada Lovelace <ada@example.com>")
	s := servertest.Start(t, "tiny")
	dir := filepath.Join(t.TempDir(), "ws")
	runOK(t, "clone", s.Addr, dir)
	if _, err := runXY("-C", dir, "commit", "-m", "x"); err == nil || !strings.Contains(err.Error(), "nothing to commit") {
		t.Errorf("clean commit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "new.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := runOK(t, "-C", filepath.Join(dir, "src"), "add", "new.go"); out != "A new.go\n" {
		t.Errorf("add: %q", out)
	}
	if out := runOK(t, "-C", dir, "status"); out != "A src/new.go\n" {
		t.Errorf("status after add: %q", out)
	}
	if _, err := runXY("-C", dir, "commit"); client.ExitCode(err) != client.ExitUsage {
		t.Errorf("commit without -m: %v", err)
	}
	out := runOK(t, "-C", dir, "commit", "-m", "add new.go")
	if len(out) < 13 || !strings.HasSuffix(out, " add new.go\n") {
		t.Errorf("commit: %q", out)
	}
	if out := runOK(t, "-C", dir, "log", "--oneline", "-n", "1"); out[:12] != strings.Fields(out)[0] || !strings.Contains(out, "add new.go") {
		t.Errorf("log after commit: %q", out)
	}
	if out := runOK(t, "-C", dir, "status"); out != "" {
		t.Errorf("status after commit: %q", out)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = runOK(t, "-C", dir, "commit", "-m", "edit", "--json")
	if !strings.Contains(out, `"changes": 1`) || !strings.Contains(out, `"uploaded_blobs": 1`) {
		t.Errorf("commit --json: %s", out)
	}
}
