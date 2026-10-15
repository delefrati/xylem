//go:build integration

package client_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/client"
)

const testAuthor = "Ada Lovelace <ada@example.com>"

// workingFiles maps each working file (outside .xylem) to its content or
// symlink target, with "+x " prefixed for executables.
func workingFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	rt, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Close() }()
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == client.MetaDir {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		info, err := rt.Lstat(rel)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := rt.Readlink(rel)
			out[filepath.ToSlash(rel)] = "-> " + target
			return err
		}
		b, err := rt.ReadFile(rel)
		prefix := ""
		if info.Mode().Perm()&0o100 != 0 {
			prefix = "+x "
		}
		out[filepath.ToSlash(rel)] = prefix + string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, w, f := cloneTiny(t)
	conn := dial(t, s.Addr)
	parent := w.State.Parent
	commit := func(msg string, all bool) (*client.CommitResult, error) {
		return w.Commit(ctx, f, client.CommitOptions{Message: []byte(msg), All: all, Author: testAuthor})
	}
	if _, err := commit("nothing\n", false); err == nil {
		t.Fatal("empty commit succeeded")
	}

	root := w.Root
	writeFile(t, root, "README", "rewritten\n")
	if err := os.Remove(filepath.Join(root, "TODO")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "src/main.go"), 0o755); err != nil { //nolint:gosec // G302: exec bit under test
		t.Fatal(err)
	}
	writeFile(t, root, "src/new.go", "package main\n")
	writeFile(t, root, "lib/util.go", "package lib\n")
	writeFile(t, root, "scratch.txt", "not committed\n")
	if err := os.Symlink("README", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add(ctx, f, [][]byte{[]byte("nope")}); client.ExitCode(err) != client.ExitUsage {
		t.Errorf("add of a missing path: %v", err)
	}
	added, err := w.Add(ctx, f, [][]byte{[]byte("src/new.go"), []byte("lib"), []byte("link")})
	if err != nil || len(added) != 3 {
		t.Fatalf("add = %q, %v", added, err)
	}
	if _, err := w.Commit(ctx, f, client.CommitOptions{Message: []byte("  \n"), Author: testAuthor}); client.ExitCode(err) != client.ExitUsage {
		t.Errorf("blank message: %v", err)
	}

	res, err := commit("first draft\n\nbody\n", false)
	if err != nil {
		t.Fatal(err)
	}
	// README, src/new.go, lib/util.go and link are new blobs; main.go only changed mode.
	if res.Changes != 6 || res.Uploaded.Blobs != 4 {
		t.Errorf("commit result %+v", res)
	}
	if w.State.Parent != res.Changeset || len(w.State.Added) != 0 {
		t.Errorf("state after commit: %+v", w.State)
	}
	if diff := cmp.Diff([]string{"? scratch.txt"}, statusOf(t, w, f)); diff != "" {
		t.Errorf("status after commit (-want +got):\n%s", diff)
	}
	cs, err := conn.Repo.GetChangesets(ctx, &xylemv1.GetChangesetsRequest{Ids: [][]byte{res.Changeset[:]}, IncludeChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	got := cs.GetChangesets()[0]
	if string(got.GetParents()[0]) != string(parent[:]) || string(got.GetAuthor().GetNameEmail()) != testAuthor ||
		string(got.GetMessage()) != "first draft\n\nbody\n" || got.GetOnMainline() {
		t.Errorf("changeset %v", got)
	}

	// A clone of the draft has exactly the committed files.
	want := workingFiles(t, root)
	delete(want, "scratch.txt")
	clone := filepath.Join(t.TempDir(), "clone")
	if _, err := client.Clone(ctx, s.Addr, clone, client.CloneOptions{Rev: res.Changeset.String()}); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, workingFiles(t, clone)); diff != "" {
		t.Errorf("clone of the draft (-want +got):\n%s", diff)
	}

	// -A includes untracked files; a second commit stacks on the first.
	res2, err := commit("second\n", true)
	if err != nil || res2.Changes != 1 || res2.Uploaded.Blobs != 1 {
		t.Fatalf("commit -A = %+v, %v", res2, err)
	}
	cs, err = conn.Repo.GetChangesets(ctx, &xylemv1.GetChangesetsRequest{Ids: [][]byte{res2.Changeset[:]}})
	if err != nil || string(cs.GetChangesets()[0].GetParents()[0]) != string(res.Changeset[:]) {
		t.Errorf("second commit parent: %v", err)
	}
	if got := statusOf(t, w, f); len(got) != 0 {
		t.Errorf("status after commit -A %q", got)
	}
	// Reopening sees the new parent; the dirstate matches it (no rebuild).
	reopened, err := client.Open(root)
	if err != nil || reopened.State.Parent != res2.Changeset {
		t.Fatalf("reopen: %v", err)
	}
	ds, err := client.LoadDirstate(reopened.Meta("dirstate"))
	if err != nil || ds.Root != reopened.State.RootTree {
		t.Errorf("dirstate root %v vs state %v (%v)", ds.Root, reopened.State.RootTree, err)
	}
}
