// Package gitsource reads git repositories by shelling out to the git binary
// (ADR-010, 08-git-import.md §Reading git): rev-list for commit order, a pool
// of long-lived `cat-file --batch` processes for objects, and diff-tree for
// changes. Paths are raw bytes, exactly as git stores them.
package gitsource

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"strings"
)

// ErrNotFound reports a missing object or ref.
var ErrNotFound = errors.New("not found in git repository")

// SHA is a git SHA-1 object name.
type SHA [20]byte

// String returns the 40-character hex form.
func (s SHA) String() string { return hex.EncodeToString(s[:]) }

// IsZero reports whether s is all zeros (git's "no object").
func (s SHA) IsZero() bool { return s == SHA{} }

// ParseSHA parses a 40-character hex SHA-1.
func ParseSHA(h string) (SHA, error) {
	var s SHA
	if len(h) != 2*len(s) {
		return s, fmt.Errorf("git sha %q: want 40 hex characters", h)
	}
	if _, err := hex.Decode(s[:], []byte(h)); err != nil {
		return s, fmt.Errorf("git sha %q: %w", h, err)
	}
	return s, nil
}

// Repo is a git repository on disk. It is safe for concurrent use.
type Repo struct {
	dir     string
	catFile *CatFilePool
}

// Options configures Open.
type Options struct {
	// CatFileProcesses is the number of long-lived `git cat-file --batch`
	// processes (default 4).
	CatFileProcesses int
}

// Open checks that dir is a SHA-1 git repository (bare or not) and starts
// the cat-file pool. Close releases it.
func Open(ctx context.Context, dir string, opts Options) (*Repo, error) {
	if opts.CatFileProcesses <= 0 {
		opts.CatFileProcesses = 4
	}
	r := &Repo{dir: dir}
	format, err := r.output(ctx, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, fmt.Errorf("%s is not a git repository: %w", dir, err)
	}
	if f := strings.TrimSpace(string(format)); f != "sha1" {
		return nil, fmt.Errorf("%s uses object format %q; only sha1 repositories are supported", dir, f)
	}
	r.catFile = newCatFilePool(r, opts.CatFileProcesses)
	return r, nil
}

// Dir returns the repository directory.
func (r *Repo) Dir() string { return r.dir }

// Close stops the cat-file processes.
func (r *Repo) Close() error { return r.catFile.Close() }

// command returns a git command in the repository. LC_ALL=C keeps messages
// stable; GIT_TERMINAL_PROMPT=0 avoids hanging on credentials.
func (r *Repo) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: running git with computed arguments is the design (ADR-010)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// output runs git and returns its standard output, with stderr in errors.
func (r *Repo) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := r.command(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ResolveBranch returns the commit refs/heads/<branch> points at.
func (r *Repo) ResolveBranch(ctx context.Context, branch string) (SHA, error) {
	out, err := r.output(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return SHA{}, fmt.Errorf("branch %q: %w", branch, ErrNotFound)
	}
	return ParseSHA(strings.TrimSpace(string(out)))
}

// RevList streams the commits reachable from tip but not from any of
// exclude, parents before children (`rev-list --topo-order --reverse`).
// Breaking out of the loop stops git.
func (r *Repo) RevList(ctx context.Context, tip SHA, exclude ...SHA) iter.Seq2[SHA, error] {
	return func(yield func(SHA, error) bool) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		args := []string{"rev-list", "--topo-order", "--reverse", tip.String()}
		for _, e := range exclude {
			args = append(args, "^"+e.String())
		}
		cmd := r.command(ctx, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			yield(SHA{}, err)
			return
		}
		if err := cmd.Start(); err != nil {
			yield(SHA{}, fmt.Errorf("git rev-list: %w", err))
			return
		}
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			sha, err := ParseSHA(sc.Text())
			if !yield(sha, err) || err != nil {
				cancel()
				_ = cmd.Wait()
				return
			}
		}
		if err := sc.Err(); err != nil {
			yield(SHA{}, fmt.Errorf("git rev-list: %w", err))
			_ = cmd.Wait()
			return
		}
		if err := cmd.Wait(); err != nil {
			yield(SHA{}, fmt.Errorf("git rev-list: %w: %s", err, strings.TrimSpace(stderr.String())))
		}
	}
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(ctx context.Context, a, b SHA) (bool, error) {
	err := r.command(ctx, "merge-base", "--is-ancestor", a.String(), b.String()).Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	default:
		return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
	}
}

// FirstParentChain returns the commits on tip's first-parent chain, tip first.
func (r *Repo) FirstParentChain(ctx context.Context, tip SHA) (map[SHA]bool, error) {
	out, err := r.output(ctx, "rev-list", "--first-parent", tip.String())
	if err != nil {
		return nil, err
	}
	chain := map[SHA]bool{}
	for _, line := range bytes.Fields(out) {
		sha, err := ParseSHA(string(line))
		if err != nil {
			return nil, err
		}
		chain[sha] = true
	}
	return chain, nil
}
