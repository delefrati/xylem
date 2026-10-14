package client

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/paths"
)

// CloneOptions configures Clone.
type CloneOptions struct {
	Repo   string   // default "default"
	Rev    string   // default "main"
	Sparse []string // sparse directories; empty for a full checkout
}

// CloneResult describes a finished clone.
type CloneResult struct {
	Workspace *Workspace
	Changeset hash.ID
	Files     int
	Fetched   FetchStats
}

// ParseSparse validates sparse directories ("a/b/" is accepted as "a/b").
func ParseSparse(dirs []string) ([]Path, error) {
	out := []Path{}
	for _, d := range dirs {
		d = strings.TrimSuffix(strings.TrimSpace(d), "/")
		if d == "" {
			continue
		}
		if err := paths.ValidPath([]byte(d)); err != nil {
			return nil, Usage(fmt.Errorf("sparse directory: %w", err))
		}
		out = append(out, Path(d))
	}
	return out, nil
}

// Clone creates a workspace in dir (which must not exist or be empty),
// resolves the revision and checks it out (06 §Commands).
func Clone(ctx context.Context, server, dir string, opts CloneOptions) (res *CloneResult, err error) {
	if opts.Repo == "" {
		opts.Repo = "default"
	}
	if opts.Rev == "" {
		opts.Rev = "main"
	}
	sparse, err := ParseSparse(opts.Sparse)
	if err != nil {
		return nil, err
	}
	created := false
	switch entries, err := os.ReadDir(dir); {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: a working tree is world-readable, like git's
			return nil, err
		}
		created = true
	case err != nil:
		return nil, err
	case len(entries) > 0:
		return nil, Usage(fmt.Errorf("destination %s exists and is not empty", dir))
	}
	defer func() {
		if err != nil && created {
			_ = os.RemoveAll(dir)
		}
	}()

	conn, err := Dial(server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	resp, err := conn.Repo.ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Repo: opts.Repo, Revision: opts.Rev})
	if err != nil {
		return nil, err
	}
	var target Target
	copy(target.Changeset[:], resp.Id)
	copy(target.Root[:], resp.RootTree)
	target.Sparse = sparse

	w, err := Init(dir, Config{Server: server, Repo: opts.Repo})
	if err != nil {
		return nil, err
	}
	f := NewFetcher(conn, opts.Repo, NewObjectCache(w.Meta("objects"), w.Config.CacheMaxBytes))
	unlock, err := w.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	co, err := w.Checkout(ctx, f, target, false)
	if err != nil {
		return nil, err
	}
	return &CloneResult{Workspace: w, Changeset: target.Changeset, Files: co.Written, Fetched: co.Fetched}, nil
}
