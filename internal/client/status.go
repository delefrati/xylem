package client

import (
	"bufio"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"golang.org/x/sync/errgroup"

	"github.com/delefrati/xylem/internal/paths"
	"github.com/delefrati/xylem/internal/treeops"
)

// Status codes (06 §Commands).
const (
	StatusModified  = 'M'
	StatusAdded     = 'A'
	StatusDeleted   = 'D'
	StatusUntracked = '?'
)

// StatusEntry is one changed path.
type StatusEntry struct {
	Code byte
	Path []byte // repository-relative
}

// Status compares the working copy (in scope) with its parent (06 §Status).
// It refreshes dirstate entries whose content still matches but whose stat
// changed, so the next run takes the stat-only path. f is used only if the
// dirstate must be rebuilt; it may be nil when the dirstate is current.
func (w *Workspace) Status(ctx context.Context, f *Fetcher) ([]StatusEntry, error) {
	ds, err := w.statusDirstate(ctx, f)
	if err != nil {
		return nil, err
	}
	files, err := w.walk(ds)
	if err != nil {
		return nil, err
	}
	added := map[string]bool{}
	for _, p := range w.State.Added {
		added[string(p)] = true
	}

	// Check tracked files in parallel; untracked ones need no hashing.
	type result struct {
		code    byte
		refresh *DirEntry
	}
	results := make([]result, len(files))
	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(runtime.GOMAXPROCS(0))
	for i, p := range files {
		e, tracked := ds.Get(p)
		if !tracked {
			if added[string(p)] {
				results[i].code = StatusAdded
			} else {
				results[i].code = StatusUntracked
			}
			continue
		}
		g.Go(func() error {
			fc, err := checkFile(w.Root, ds, p)
			if err != nil {
				return err
			}
			switch {
			case !fc.clean:
				results[i].code = StatusModified
			case !fc.stat: // clean by content, stale stat: refresh it
				fresh := entryFor(p, fc.info, fc.mode, e.Blob)
				results[i].refresh = &fresh
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	var out []StatusEntry
	seen := make(map[string]bool, len(files))
	refreshed := false
	for i, p := range files {
		seen[string(p)] = true
		if results[i].code != 0 {
			out = append(out, StatusEntry{Code: results[i].code, Path: p})
		}
		if r := results[i].refresh; r != nil {
			ds.Set(*r)
			refreshed = true
		}
	}
	for _, e := range ds.Entries() {
		if !seen[string(e.Path)] {
			out = append(out, StatusEntry{Code: StatusDeleted, Path: e.Path})
		}
	}
	slices.SortFunc(out, func(a, b StatusEntry) int { return treeops.ComparePathsDFS(a.Path, b.Path) })
	if refreshed {
		if err := ds.Save(w.Meta("dirstate")); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (w *Workspace) statusDirstate(ctx context.Context, f *Fetcher) (*Dirstate, error) {
	ds, err := LoadDirstate(w.Meta("dirstate"))
	if err == nil && ds.Root == w.State.RootTree {
		return ds, nil
	}
	if f == nil {
		return nil, errors.New("the dirstate must be rebuilt, which needs the server")
	}
	if ds, err = w.LoadDirstateFor(ctx, f); err != nil {
		return nil, err
	}
	return ds, ds.Save(w.Meta("dirstate"))
}

// ignoreRules accumulates .gitignore patterns while walking down.
type ignoreRules struct {
	patterns []gitignore.Pattern
	matcher  gitignore.Matcher
}

func (r ignoreRules) with(dirAbs string, dir []string) ignoreRules {
	f, err := os.Open(filepath.Join(dirAbs, ".gitignore"))
	if err != nil {
		return r
	}
	defer func() { _ = f.Close() }()
	ps := slices.Clone(r.patterns)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ps = append(ps, gitignore.ParsePattern(line, dir))
	}
	return ignoreRules{patterns: ps, matcher: gitignore.NewMatcher(ps)}
}

func (r ignoreRules) ignored(path []string, isDir bool) bool {
	return r.matcher != nil && r.matcher.Match(path, isDir)
}

// walk lists the working files in scope that are tracked, or untracked and
// not ignored (tracked files are listed even if a rule matches them).
// Directories outside the scope and .xylem are not entered.
func (w *Workspace) walk(ds *Dirstate) ([][]byte, error) {
	scope := w.Scope()
	var out [][]byte
	var visit func(abs string, rel []string, rules ignoreRules) error
	visit = func(abs string, rel []string, rules ignoreRules) error {
		rules = rules.with(abs, rel)
		entries, err := os.ReadDir(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		for _, e := range entries {
			name := e.Name()
			if len(rel) == 0 && name == MetaDir {
				continue
			}
			childRel := append(slices.Clone(rel), name)
			p := []byte(strings.Join(childRel, "/"))
			isDir := e.IsDir() // symlinks to directories are files here, as in git
			if isDir {
				if !scope.TreeInScope(p) || rules.ignored(childRel, true) && !hasTrackedUnder(ds, p) {
					continue
				}
				if err := visit(filepath.Join(abs, name), childRel, rules); err != nil {
					return err
				}
				continue
			}
			if !scope.FileInScope(p) {
				continue
			}
			if rules.ignored(childRel, false) {
				if _, tracked := ds.Get(p); !tracked {
					continue
				}
			}
			out = append(out, p)
		}
		return nil
	}
	if err := visit(w.Root, nil, ignoreRules{}); err != nil {
		return nil, err
	}
	return out, nil
}

// hasTrackedUnder reports whether the dirstate tracks anything under dir
// (an ignored directory with tracked files must still be walked).
func hasTrackedUnder(ds *Dirstate, dir []byte) bool {
	for _, e := range ds.entries {
		if paths.IsUnder(e.Path, dir) {
			return true
		}
	}
	return false
}
