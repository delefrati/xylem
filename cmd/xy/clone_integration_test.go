//go:build integration

package main

import (
	"path/filepath"
	"strings"
	"testing"

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
