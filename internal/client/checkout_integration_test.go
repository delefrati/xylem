//go:build integration

package client_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/server/servertest"
)

// assertMatchesGit compares the working tree (in scope) with git at sha:
// file set, content, exec bit and symlink targets; .xylem is ignored.
func assertMatchesGit(t *testing.T, root string, s *servertest.Server, sha string, inScope func(string) bool) {
	t.Helper()
	ctx := context.Background()
	g, _ := gitsource.ParseSHA(sha)
	entries, err := s.Git.LsTree(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]gitsource.TreeEntry{}
	for _, e := range entries {
		if e.Mode != gitsource.ModeSubmodule && (inScope == nil || inScope(string(e.Path))) {
			want[string(e.Path)] = e
		}
	}
	var found []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == client.MetaDir {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, rel := range found {
		p := filepath.Join(root, rel)
		got[rel] = true
		e, ok := want[rel]
		if !ok {
			t.Errorf("%s: unexpected file %q", sha[:8], rel)
			continue
		}
		gitContent, _ := s.Git.Blob(ctx, e.SHA)
		info, _ := os.Lstat(p)
		switch e.Mode {
		case gitsource.ModeSymlink:
			target, err := os.Readlink(p)
			if err != nil || target != string(gitContent) {
				t.Errorf("%s: %q symlink target %q, git %q", sha[:8], rel, target, gitContent)
			}
		default:
			content, _ := os.ReadFile(p)
			if string(content) != string(gitContent) {
				t.Errorf("%s: %q content differs", sha[:8], rel)
			}
			if exec := info.Mode().Perm()&0o100 != 0; exec != (e.Mode == gitsource.ModeExec) {
				t.Errorf("%s: %q exec bit %v, git mode %o", sha[:8], rel, exec, e.Mode)
			}
		}
	}
	for p := range want {
		if !got[p] {
			t.Errorf("%s: missing file %q", sha[:8], p)
		}
	}
}

// resolve returns a changeset and root for a git sha.
func resolve(t *testing.T, c *client.Conn, gitSHA string) client.Target {
	t.Helper()
	resp, err := c.Repo.ResolveRevision(context.Background(), &xylemv1.ResolveRevisionRequest{Revision: "git:" + gitSHA})
	if err != nil {
		t.Fatal(err)
	}
	var tgt client.Target
	copy(tgt.Changeset[:], resp.Id)
	copy(tgt.Root[:], resp.RootTree)
	return tgt
}

func newWorkspace(t *testing.T, s *servertest.Server) (*client.Workspace, *client.Fetcher) {
	t.Helper()
	dir := t.TempDir()
	w, err := client.Init(dir, client.Config{Server: s.Addr})
	if err != nil {
		t.Fatal(err)
	}
	conn := dial(t, s.Addr)
	return w, client.NewFetcher(conn, "default", client.NewObjectCache(w.Meta("objects"), 0))
}

func TestCheckoutAcrossRevisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := servertest.Start(t, "edge")
	w, f := newWorkspace(t, s)
	conn := dial(t, s.Addr)
	commits := strings.Fields(gitOut(t, s.GitDir, "rev-list", "--first-parent", "main"))

	// Walk the whole first-parent history, oldest to newest and back to the
	// middle: covers file ↔ symlink ↔ directory changes, exec flips, deletes.
	order := make([]string, 0, len(commits)+3)
	for i := len(commits) - 1; i >= 0; i-- {
		order = append(order, commits[i])
	}
	order = append(order, commits[len(commits)/2], commits[len(commits)-1], commits[0])
	for _, sha := range order {
		if _, err := w.Checkout(ctx, f, resolve(t, conn, sha), false); err != nil {
			t.Fatalf("checkout %s: %v", sha[:8], err)
		}
		assertMatchesGit(t, w.Root, s, sha, nil)
		reopened, err := client.Open(w.Root)
		if err != nil || reopened.State.Parent != resolve(t, conn, sha).Changeset {
			t.Fatalf("state after checkout %s: %v", sha[:8], err)
		}
	}
}

func TestCheckoutRefusesDirty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := servertest.Start(t, "tiny")
	w, f := newWorkspace(t, s)
	conn := dial(t, s.Addr)
	commits := strings.Fields(gitOut(t, s.GitDir, "rev-list", "main"))
	tip, older := commits[0], commits[3]
	if _, err := w.Checkout(ctx, f, resolve(t, conn, tip), false); err != nil {
		t.Fatal(err)
	}
	// README differs between the two revisions; edit it locally.
	if err := os.WriteFile(filepath.Join(w.Root, "README"), []byte("local edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := w.Checkout(ctx, f, resolve(t, conn, older), false)
	var dirty *client.DirtyError
	if client.ExitCode(err) != client.ExitDirty || !errors.As(err, &dirty) || string(dirty.Paths[0]) != "README" {
		t.Fatalf("dirty checkout = %v (code %d)", err, client.ExitCode(err))
	}
	assertNotMoved := func() {
		reopened, _ := client.Open(w.Root)
		if reopened.State.Parent != resolve(t, conn, tip).Changeset {
			t.Fatal("a refused checkout moved the state")
		}
	}
	assertNotMoved()
	// An untracked file where the target has a file is also refused.
	if err := os.Remove(filepath.Join(w.Root, "README")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Checkout(ctx, f, resolve(t, conn, tip), true); err != nil { // restore
		t.Fatal(err)
	}
	notes := filepath.Join(w.Root, "notes.txt") // exists in older revisions only
	if err := os.WriteFile(notes, []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Checkout(ctx, f, resolve(t, conn, older), false); client.ExitCode(err) != client.ExitDirty {
		t.Fatalf("untracked file in the way: %v", err)
	}
	// --force discards local changes.
	if _, err := w.Checkout(ctx, f, resolve(t, conn, older), true); err != nil {
		t.Fatalf("forced checkout: %v", err)
	}
	assertMatchesGit(t, w.Root, s, older, nil)
}

func TestCheckoutDirectoryInTheWay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := servertest.Start(t, "edge")
	w, f := newWorkspace(t, s)
	conn := dial(t, s.Addr)
	sub := func(subject string) string {
		return strings.TrimSpace(gitOut(t, s.GitDir, "log", "--format=%H", "--fixed-strings", "--grep", subject, "-n1", "main"))
	}
	asDir, asFile := sub("x: symlink -> directory"), sub("x: directory -> file")
	if _, err := w.Checkout(ctx, f, resolve(t, conn, asDir), false); err != nil {
		t.Fatal(err)
	}
	// An untracked file inside x would be lost when x becomes a file.
	if err := os.WriteFile(filepath.Join(w.Root, "x", "untracked.txt"), []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := w.Checkout(ctx, f, resolve(t, conn, asFile), false)
	var dirty *client.DirtyError
	if !errors.As(err, &dirty) || len(dirty.Paths) != 1 || string(dirty.Paths[0]) != "x/untracked.txt" {
		t.Fatalf("directory with an untracked file in the way: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.Root, "x", "inner.txt")); err != nil {
		t.Error("a refused checkout changed files")
	}
	if _, err := w.Checkout(ctx, f, resolve(t, conn, asFile), true); err != nil {
		t.Fatalf("forced: %v", err)
	}
	assertMatchesGit(t, w.Root, s, asFile, nil)
}
