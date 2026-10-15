package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/client"
)

// workdir is -C, or the current directory.
func (g *globals) workdir() (string, error) {
	if g.dir != "" {
		return g.dir, nil
	}
	return os.Getwd()
}

// workspace opens the workspace containing the working directory.
func (g *globals) workspace() (*client.Workspace, error) {
	dir, err := g.workdir()
	if err != nil {
		return nil, err
	}
	return client.Open(dir)
}

// cache returns a workspace's local object cache.
func cacheOf(w *client.Workspace) *client.ObjectCache {
	return w.Cache()
}

// emit prints v as JSON with --json, or calls human otherwise.
func (g *globals) emit(cmd *cobra.Command, v any, human func(w io.Writer) error) error {
	if g.json {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	return human(cmd.OutOrStdout())
}

func printf(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format, args...)
	return err
}

// serverAddr and repoName are the flag values, or the workspace's config.
func (g *globals) serverAddr(w *client.Workspace) string {
	if g.server != "" || w == nil {
		return g.server
	}
	return w.Config.Server
}

func (g *globals) repoName(w *client.Workspace) string {
	if g.repo != "" || w == nil {
		return g.repo
	}
	return w.Config.Repo
}

// fetcher dials the server (lazily: no I/O until the first call) and returns
// a fetcher over the workspace cache. Close the connection when done.
func (g *globals) fetcher(w *client.Workspace) (*client.Conn, *client.Fetcher, error) {
	conn, err := client.Dial(g.serverAddr(w))
	if err != nil {
		return nil, nil, err
	}
	return conn, client.NewFetcher(conn, g.repoName(w), cacheOf(w)), nil
}

// displayPath renders a repository path relative to the working directory,
// quoting it if it is not valid UTF-8.
func (g *globals) displayPath(w *client.Workspace, p []byte) string {
	s := string(p)
	if cwd, err := g.workdir(); err == nil {
		if abs, err := filepath.Abs(cwd); err == nil {
			if rel, err := filepath.Rel(abs, filepath.Join(w.Root, filepath.FromSlash(s))); err == nil {
				s = filepath.ToSlash(rel)
			}
		}
	}
	if !utf8.ValidString(s) {
		return strconv.Quote(s)
	}
	return s
}

// repoPaths converts command-line paths (relative to the working directory)
// to repository paths. "." at the workspace root is the empty (root) path.
func (g *globals) repoPaths(w *client.Workspace, args []string) ([][]byte, error) {
	cwd, err := g.workdir()
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(args))
	for _, a := range args {
		abs := a
		if !filepath.IsAbs(a) {
			abs = filepath.Join(cwd, a)
		}
		abs, err = filepath.Abs(abs)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(w.Root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return nil, client.Usage(fmt.Errorf("path %q is outside the workspace", a))
		}
		if rel == "." {
			rel = ""
		}
		out = append(out, []byte(filepath.ToSlash(rel)))
	}
	return out, nil
}
