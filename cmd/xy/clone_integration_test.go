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

func TestCloneCommand(t *testing.T) {
	t.Parallel()
	s := servertest.Start(t, "tiny")
	dir := filepath.Join(t.TempDir(), "ws")
	out, err := runXY("clone", s.Addr, dir, "--json")
	if err != nil || !strings.Contains(out, `"files": 4`) || !strings.Contains(out, s.TipID.String()) {
		t.Fatalf("clone: %q, %v", out, err)
	}
	if _, err := runXY("clone", s.Addr); err == nil {
		t.Error("clone with one argument succeeded")
	}
}

func TestStatusAndDiffCommands(t *testing.T) {
	t.Parallel()
	s := servertest.Start(t, "tiny")
	dir := filepath.Join(t.TempDir(), "ws")
	if _, err := runXY("clone", s.Addr, dir); err != nil {
		t.Fatal(err)
	}
	if out, err := runXY("-C", dir, "status"); err != nil || out != "" {
		t.Fatalf("clean status: %q, %v", out, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "new.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	// Paths are relative to the working directory.
	if out, err := runXY("-C", src, "status"); err != nil || out != "M ../README\n? new.go\n" {
		t.Errorf("status in src: %q, %v", out, err)
	}
	out, err := runXY("-C", dir, "status", "--json")
	if err != nil || !strings.Contains(out, `"status": "M"`) || !strings.Contains(out, `"path": "src/new.go"`) {
		t.Errorf("status --json: %q, %v", out, err)
	}
	if out, err := runXY("-C", src, "diff", ".."); err != nil || !strings.Contains(out, "+++ b/README\n") || !strings.Contains(out, "+edited\n") {
		t.Errorf("diff: %q, %v", out, err)
	}
	if out, err := runXY("-C", src, "diff", "."); err != nil || out != "" {
		t.Errorf("diff of src: %q, %v", out, err)
	}
	if _, err := runXY("-C", src, "diff", "../.."); client.ExitCode(err) != client.ExitUsage {
		t.Errorf("diff outside the workspace: %v", err)
	}
}
