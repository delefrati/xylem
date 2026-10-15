package client

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/delefrati/xylem/internal/hash"
)

func TestInitOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := Init(dir, Config{Server: "localhost:7400"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Config.Repo != "default" || w.Config.CacheMaxBytes != DefaultCacheMaxBytes {
		t.Errorf("defaults: %+v", w.Config)
	}
	if _, err := Init(dir, Config{}); err == nil {
		t.Error("second Init succeeded")
	}
	w.State.Parent = hash.Sum(nil, []byte("p"))
	w.State.RootTree = hash.Sum(nil, []byte("r"))
	w.State.Sparse = []Path{Path("services/api"), Path("caf\xe9")}
	w.State.Added = []Path{Path("new/file.go")}
	if err := w.SaveState(); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	got, err := Open(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != w.Root {
		t.Errorf("Root = %s, want %s", got.Root, w.Root)
	}
	if diff := cmp.Diff(w.State, got.State); diff != "" {
		t.Errorf("state mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(w.Config, got.Config); diff != "" {
		t.Errorf("config mismatch (-want +got):\n%s", diff)
	}
	raw, _ := os.ReadFile(w.Meta("state.json"))
	if !strings.Contains(string(raw), `"services/api"`) || !strings.Contains(string(raw), `"base64": "Y2Fm6Q=="`) || !strings.Contains(string(raw), w.State.Parent.String()) {
		t.Errorf("state.json:\n%s", raw)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, MetaDir))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".xy-tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if _, err := Open(t.TempDir()); !errors.Is(err, ErrNotWorkspace) {
		t.Errorf("Open outside a workspace: %v", err)
	}
}

func TestOpenRejectsFutureState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := Init(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	w.State.Version = 99
	if err := w.SaveState(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Error("future state version accepted")
	}
}

func TestPathJSON(t *testing.T) {
	t.Parallel()
	for _, p := range []Path{Path("plain/path"), Path("\xff\xfe"), Path("")} {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var got Path
		if err := json.Unmarshal(b, &got); err != nil || string(got) != string(p) {
			t.Errorf("%q → %s → %q, %v", p, b, got, err)
		}
	}
	var p Path
	for _, bad := range []string{`42`, `{"base64": "!!"}`} {
		if err := json.Unmarshal([]byte(bad), &p); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := Init(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := w.Lock()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := Open(dir)
	if _, err := other.Lock(); !errors.Is(err, ErrLocked) {
		t.Errorf("second Lock = %v, want ErrLocked", err)
	}
	unlock()
	again, err := other.Lock()
	if err != nil {
		t.Errorf("Lock after unlock: %v", err)
	}
	again()
}

func TestDialAddresses(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "unix://", "unix://relative.sock"} {
		if _, err := Dial(bad, ""); err == nil {
			t.Errorf("Dial(%q) succeeded", bad)
		}
	}
	for _, good := range []string{"localhost:7400", "unix:///run/user/1000/xylem/daemon.sock"} {
		c, err := Dial(good, "") // lazy: no connection is made
		if err != nil {
			t.Errorf("Dial(%q): %v", good, err)
			continue
		}
		_ = c.Close()
	}
}

func TestNoCacheConfig(t *testing.T) {
	t.Parallel()
	w, err := Init(t.TempDir(), Config{Server: "x:1", CacheMaxBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(w.Root)
	if err != nil || reopened.Config.CacheMaxBytes != 0 {
		t.Fatalf("cache_max_bytes = %d, %v", reopened.Config.CacheMaxBytes, err)
	}
	if s, _ := reopened.Cache().Stats(); s.MaxBytes >= 0 {
		t.Errorf("a 0 limit in config.toml should mean no persistent cache, got limit %d", s.MaxBytes)
	}
	d, err := Init(t.TempDir(), Config{Server: "x:1"})
	if err != nil || d.Config.CacheMaxBytes != DefaultCacheMaxBytes {
		t.Errorf("default limit %d, %v", d.Config.CacheMaxBytes, err)
	}
}
