package client

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
)

func TestDirstateRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "dirstate")
	if _, err := LoadDirstate(p); !errors.Is(err, ErrNoDirstate) {
		t.Errorf("missing dirstate: %v", err)
	}
	d := NewDirstate()
	d.Set(DirEntry{Path: []byte("b"), Mode: object.ModeExec, Size: 3, MtimeNs: 42, Blob: hash.ID{1}})
	d.Set(DirEntry{Path: []byte("a/\xff"), Mode: object.ModeSymlink, Size: 1, MtimeNs: 7, Blob: hash.ID{2}})
	d.Set(DirEntry{Path: []byte("gone"), Mode: object.ModeFile})
	d.Delete([]byte("gone"))
	if err := d.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDirstate(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.WrittenAtNs != d.WrittenAtNs || got.Len() != 2 {
		t.Fatalf("loaded %d entries, written at %d", got.Len(), got.WrittenAtNs)
	}
	es := got.Entries()
	if string(es[0].Path) != "a/\xff" || es[1].Mode != object.ModeExec || es[1].Blob != (hash.ID{1}) {
		t.Errorf("entries = %+v", es)
	}
	raw, _ := os.ReadFile(p)
	if string(raw[:6]) != "XYLDS1" {
		t.Errorf("header %q", raw[:6])
	}
	if err := os.WriteFile(p, []byte("XYLDS9junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDirstate(p); !errors.Is(err, ErrNoDirstate) {
		t.Errorf("bad header: %v", err)
	}
	if err := os.WriteFile(p, []byte("XYLDS1junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDirstate(p); !errors.Is(err, ErrNoDirstate) {
		t.Errorf("bad body: %v", err)
	}
}

func TestRebuildDirstateAndHashing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(p, content string, perm os.FileMode) hash.ID {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(content), perm); err != nil {
			t.Fatal(err)
		}
		return object.ID(object.KindBlob, []byte(content))
	}
	same := write("same.txt", "same", 0o644)
	exe := write("bin/run", "#!/bin/sh\n", 0o755)
	_ = write("changed.txt", "new content", 0o644)
	if err := os.Symlink("same.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	files := []FileEntry{
		{Path: []byte("same.txt"), Mode: object.ModeFile, ID: same, Size: 4},
		{Path: []byte("bin/run"), Mode: object.ModeExec, ID: exe, Size: 10},
		{Path: []byte("changed.txt"), Mode: object.ModeFile, ID: object.ID(object.KindBlob, []byte("old")), Size: 3},
		{Path: []byte("link"), Mode: object.ModeSymlink, ID: object.ID(object.KindBlob, []byte("same.txt")), Size: 8},
		{Path: []byte("missing.txt"), Mode: object.ModeFile, ID: hash.ID{9}, Size: 1},
	}
	d, err := RebuildDirstate(root, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"same.txt", "bin/run", "link"} {
		if e, _ := d.Get([]byte(p)); e.MtimeNs == 0 {
			t.Errorf("%s matches but has no stat", p)
		}
	}
	for _, p := range []string{"changed.txt", "missing.txt"} {
		if e, ok := d.Get([]byte(p)); !ok || e.MtimeNs != 0 {
			t.Errorf("%s should be tracked with MtimeNs 0, got %+v", p, e)
		}
	}
	info, _ := os.Lstat(filepath.Join(root, "bin/run"))
	if m, _ := FileMode(info); m != object.ModeExec {
		t.Errorf("exec mode = %v", m)
	}
	dirInfo, _ := os.Lstat(filepath.Join(root, "bin"))
	if _, ok := FileMode(dirInfo); ok {
		t.Error("directory has a file mode")
	}
}
