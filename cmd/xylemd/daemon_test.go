package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareSocket(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "run", "xylem")
	sock := filepath.Join(dir, "d.sock")
	if err := prepareSocket(sock); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir: %v %v", fi, err)
	}
	// A live socket is refused.
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareSocket(sock); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Errorf("live socket: %v", err)
	}
	// A stale one (its daemon died without unlinking) is removed.
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatal("the stale socket file should still exist")
	}
	if err := prepareSocket(sock); err != nil {
		t.Errorf("stale socket: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("stale socket not removed: %v", err)
	}
}
