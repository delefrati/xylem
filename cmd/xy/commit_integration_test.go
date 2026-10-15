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

// Not parallel: the author comes from XYLEM_AUTHOR.
func TestStackSyncLandCommands(t *testing.T) {
	t.Setenv("XYLEM_AUTHOR", "Ada Lovelace <ada@example.com>")
	s := servertest.Start(t, "tiny")
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	runOK(t, "clone", s.Addr, a)
	runOK(t, "clone", s.Addr, b)
	write := func(dir, rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if out := runOK(t, "-C", a, "stack"); out != "" {
		t.Errorf("empty stack: %q", out)
	}
	write(a, "README", "a\n")
	runOK(t, "-C", a, "commit", "-m", "a one")
	write(a, "TODO", "a todo\n")
	runOK(t, "-C", a, "commit", "-m", "a two")
	out := runOK(t, "-C", a, "stack")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 || !strings.HasSuffix(lines[0], " a two") || !strings.HasSuffix(lines[1], " a one") {
		t.Errorf("stack: %q", out)
	}
	if out := runOK(t, "-C", a, "land"); !strings.HasPrefix(out, "landed 2 changesets (0 already on main); @ at ") {
		t.Errorf("land: %q", out)
	}

	// b edits README too: sync and land both report the conflict (exit 3).
	write(b, "README", "b\n")
	runOK(t, "-C", b, "commit", "-m", "b one")
	out, err := runXY("-C", b, "land")
	if client.ExitCode(err) != client.ExitConflict || !strings.Contains(out, "content         README\n") {
		t.Errorf("conflicting land: %q, %v", out, err)
	}
	out, err = runXY("-C", b, "sync", "--json")
	if client.ExitCode(err) != client.ExitConflict || !strings.Contains(out, `"kind": "content"`) || !strings.Contains(out, `"path": "README"`) {
		t.Errorf("conflicting sync --json: %q, %v", out, err)
	}
	if got, _ := os.ReadFile(filepath.Join(b, "README")); string(got) != "b\n" {
		t.Errorf("README after the conflict: %q", got)
	}

	// A fresh workspace behind main syncs to it.
	c := filepath.Join(t.TempDir(), "c")
	runOK(t, "clone", s.Addr, c, "--rev", "main~1")
	if out := runOK(t, "-C", c, "sync"); !strings.HasPrefix(out, "@ at ") || !strings.HasSuffix(out, " (main)\n") {
		t.Errorf("sync with no drafts: %q", out)
	}
	if got, _ := os.ReadFile(filepath.Join(c, "TODO")); string(got) != "a todo\n" {
		t.Errorf("TODO after sync: %q", got)
	}
	write(c, "README", "dirty\n")
	if _, err := runXY("-C", c, "sync"); client.ExitCode(err) != client.ExitDirty {
		t.Errorf("dirty sync: %v", err)
	}
}
