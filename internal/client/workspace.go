package client

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/delefrati/xylem/internal/hash"
)

// MetaDir is the workspace metadata directory.
const MetaDir = ".xylem"

// DefaultCacheMaxBytes bounds the local object cache (06 §Workspace layout).
const DefaultCacheMaxBytes = 4 << 30

// ErrNotWorkspace reports a directory that is not inside a workspace.
var ErrNotWorkspace = errors.New("not inside a xylem workspace (no .xylem directory found)")

// ErrLocked reports a workspace another xy command is changing.
var ErrLocked = errors.New("workspace is locked by another xy command")

// Config is .xylem/config.toml.
type Config struct {
	Server        string `toml:"server"`
	Repo          string `toml:"repo"`
	CacheMaxBytes int64  `toml:"cache_max_bytes"` // 0: no persistent cache (use with a daemon)
	Token         string `toml:"token"`           // bearer token (M9.1); XYLEM_TOKEN overrides
}

// Path is a repository path in JSON: a string when it is valid UTF-8, or
// {"base64": "…"} otherwise, since JSON strings cannot carry arbitrary bytes
// (paths are raw bytes, ADR-015).
type Path []byte

// MarshalJSON implements json.Marshaler.
func (p Path) MarshalJSON() ([]byte, error) {
	if utf8.Valid(p) {
		return json.Marshal(string(p))
	}
	return json.Marshal(struct {
		Base64 string `json:"base64"`
	}{base64.StdEncoding.EncodeToString(p)})
}

// UnmarshalJSON implements json.Unmarshaler.
func (p *Path) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*p = Path(s)
		return nil
	}
	var obj struct {
		Base64 string `json:"base64"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("path: want a string or {\"base64\": …}: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(obj.Base64)
	if err != nil {
		return fmt.Errorf("path: %w", err)
	}
	*p = raw
	return nil
}

// StateVersion is the state.json format version.
const StateVersion = 1

// State is .xylem/state.json (06 §Workspace layout).
type State struct {
	Version  int     `json:"version"`
	Parent   hash.ID `json:"parent"`    // the working copy is based on this changeset
	RootTree hash.ID `json:"root_tree"` // its root tree
	Sparse   []Path  `json:"sparse"`    // empty: full checkout
	Added    []Path  `json:"added"`     // untracked paths marked for the next commit
}

// Workspace is an xy working copy.
type Workspace struct {
	Root   string // the directory containing .xylem
	Config Config
	State  State
}

// Meta returns the path of name inside .xylem.
func (w *Workspace) Meta(name string) string {
	return filepath.Join(w.Root, MetaDir, name)
}

// Cache returns the workspace's local object cache. cache_max_bytes = 0 in
// config.toml means no persistent cache (objects are dropped after each
// command; a daemon is expected to cache them).
func (w *Workspace) Cache() *ObjectCache {
	limit := w.Config.CacheMaxBytes
	if limit == 0 {
		limit = -1
	}
	return NewObjectCache(w.Meta("objects"), limit)
}

// Init creates a workspace in dir, which must exist. cfg.CacheMaxBytes 0
// means DefaultCacheMaxBytes and a negative value no persistent cache.
func Init(dir string, cfg Config) (*Workspace, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if cfg.Repo == "" {
		cfg.Repo = "default"
	}
	switch {
	case cfg.CacheMaxBytes == 0:
		cfg.CacheMaxBytes = DefaultCacheMaxBytes
	case cfg.CacheMaxBytes < 0: // no persistent cache (stored as 0)
		cfg.CacheMaxBytes = 0
	}
	w := &Workspace{Root: root, Config: cfg, State: State{Version: StateVersion, Sparse: []Path{}, Added: []Path{}}}
	if err := os.Mkdir(filepath.Join(root, MetaDir), 0o750); err != nil {
		return nil, fmt.Errorf("creating workspace: %w", err)
	}
	if err := os.Mkdir(w.Meta("objects"), 0o750); err != nil {
		return nil, err
	}
	if err := w.SaveConfig(); err != nil {
		return nil, err
	}
	if err := w.SaveState(); err != nil {
		return nil, err
	}
	return w, nil
}

// Open finds the workspace containing start (walking up) and loads it.
func Open(start string) (*Workspace, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, MetaDir)); err == nil && fi.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, ErrNotWorkspace
		}
		dir = parent
	}
	w := &Workspace{Root: dir}
	if _, err := toml.DecodeFile(w.Meta("config.toml"), &w.Config); err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	b, err := os.ReadFile(w.Meta("state.json"))
	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}
	if err := json.Unmarshal(b, &w.State); err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}
	if w.State.Version != StateVersion {
		return nil, fmt.Errorf("state.json version %d, this xy understands %d", w.State.Version, StateVersion)
	}
	return w, nil
}

// SaveState replaces state.json atomically.
func (w *Workspace) SaveState() error {
	b, err := json.MarshalIndent(w.State, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(w.Meta("state.json"), append(b, '\n'), 0o644)
}

// SaveConfig replaces config.toml atomically.
func (w *Workspace) SaveConfig() error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(w.Config); err != nil {
		return err
	}
	return WriteFileAtomic(w.Meta("config.toml"), buf.Bytes(), 0o600) // may hold a token
}

// WriteFileAtomic writes data to a temp file in path's directory, fsyncs it,
// sets perm and renames it over path, so readers see the old or the new
// content, never a mix.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".xy-tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Lock takes the workspace lock (flock on .xylem/lock) for a mutating
// command. It does not wait: a held lock fails with ErrLocked.
func (w *Workspace) Lock() (unlock func(), err error) {
	f, err := os.OpenFile(w.Meta("lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // G115: a file descriptor fits int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("locking workspace: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // G115: a file descriptor fits int
		_ = f.Close()
	}, nil
}
