//go:build integration

package client_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/server/servertest"
)

func statusOf(t *testing.T, w *client.Workspace, f *client.Fetcher) []string {
	t.Helper()
	st, err := w.Status(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, s := range st {
		out = append(out, string(s.Code)+" "+string(s.Path))
	}
	return out
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil { //nolint:gosec // G703: a test-controlled path
		t.Fatal(err)
	}
}

func cloneTiny(t *testing.T) (*servertest.Server, *client.Workspace, *client.Fetcher) {
	t.Helper()
	s := servertest.Start(t, "tiny")
	dir := filepath.Join(t.TempDir(), "ws")
	res, err := client.Clone(context.Background(), s.Addr, dir, client.CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s, res.Workspace, client.NewFetcher(dial(t, s.Addr), "default", client.NewObjectCache(res.Workspace.Meta("objects"), 0))
}

func TestStatus(t *testing.T) {
	t.Parallel()
	_, w, f := cloneTiny(t)
	if got := statusOf(t, w, f); len(got) != 0 {
		t.Fatalf("fresh clone status %q", got)
	}
	root := w.Root
	writeFile(t, root, "README", "local edit\n")
	if err := os.Remove(filepath.Join(root, "TODO")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "src/main.go"), 0o755); err != nil { //nolint:gosec // G302: exec bit under test
		t.Fatal(err)
	}
	writeFile(t, root, "src/new.go", "package main\n")
	writeFile(t, root, "src/added.go", "package main\n")
	w.State.Added = []client.Path{client.Path("src/added.go")}
	// Ignore rules: root and nested .gitignore, a negation, an ignored
	// directory, and a tracked file matching a rule (still checked).
	writeFile(t, root, ".gitignore", "*.log\nbuild/\n!keep.log\n")
	writeFile(t, root, "debug.log", "x")
	writeFile(t, root, "keep.log", "x")
	writeFile(t, root, "build/out.bin", "x")
	writeFile(t, root, "docs/.gitignore", "*.md\n")
	writeFile(t, root, "docs/draft.md", "x")
	writeFile(t, root, "docs/index.md", "changed\n") // tracked, ignored by pattern
	writeFile(t, root, "src/notes.md", "x")          // docs/.gitignore does not apply here

	want := []string{
		"? .gitignore", "M README", "D TODO",
		"? docs/.gitignore", "M docs/index.md", "? keep.log",
		"A src/added.go", "M src/main.go", "? src/new.go", "? src/notes.md",
	}
	if diff := cmp.Diff(want, statusOf(t, w, f)); diff != "" {
		t.Errorf("status (-want +got):\n%s", diff)
	}
}

// TestStatusRacy: a file modified within the same mtime as its dirstate
// entry, at or after the dirstate write, is hashed rather than trusted.
func TestStatusRacy(t *testing.T) {
	t.Parallel()
	_, w, f := cloneTiny(t)
	readme := filepath.Join(w.Root, "README")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(readme, future, future); err != nil {
		t.Fatal(err)
	}
	// Clean by content: the entry is refreshed with the future mtime.
	if got := statusOf(t, w, f); len(got) != 0 {
		t.Fatalf("touched file status %q", got)
	}
	ds, err := client.LoadDirstate(w.Meta("dirstate"))
	if err != nil {
		t.Fatal(err)
	}
	e, _ := ds.Get([]byte("README"))
	if e.MtimeNs != future.UnixNano() {
		t.Fatalf("entry not refreshed: mtime %d", e.MtimeNs)
	}
	// Same size and mtime, different content: only hashing can tell.
	content, _ := os.ReadFile(readme)
	altered := bytes.ToUpper(content)
	if bytes.Equal(altered, content) {
		t.Fatal("fixture README has no lower-case letters")
	}
	if err := os.WriteFile(readme, altered, 0o600); err != nil { //nolint:gosec // G703: a test-controlled path
		t.Fatal(err)
	}
	if err := os.Chtimes(readme, future, future); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"M README"}, statusOf(t, w, f)); diff != "" {
		t.Errorf("racy status (-want +got):\n%s", diff)
	}
}

func TestWorkingDiff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, w, f := cloneTiny(t)
	root := w.Root
	orig, _ := os.ReadFile(filepath.Join(root, "README"))
	writeFile(t, root, "README", string(orig)+"more\n")
	if err := os.Remove(filepath.Join(root, "TODO")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "src/main.go"), 0o755); err != nil { //nolint:gosec // G302: exec bit under test
		t.Fatal(err)
	}
	writeFile(t, root, "src/added.go", "package main\n")
	writeFile(t, root, "untracked", "not shown\n")
	w.State.Added = []client.Path{client.Path("src/added.go")}

	var out bytes.Buffer
	if err := w.WriteDiff(ctx, f, &out, nil, 3); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"diff --xy a/README b/README\n--- a/README\n+++ b/README\n@@ ",
		"+more\n",
		"diff --xy a/TODO b/TODO\ndeleted file mode 100644\n--- a/TODO\n+++ /dev/null\n",
		"diff --xy a/src/added.go b/src/added.go\nnew file mode 100644\n--- /dev/null\n+++ b/src/added.go\n@@ -0,0 +1 @@\n+package main\n",
		"diff --xy a/src/main.go b/src/main.go\nold mode 100644\nnew mode 100755\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diff lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "untracked") {
		t.Errorf("diff shows an untracked file:\n%s", got)
	}

	// A path filter.
	out.Reset()
	if err := w.WriteDiff(ctx, f, &out, [][]byte{[]byte("src")}, 3); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "README") || !strings.Contains(out.String(), "src/main.go") {
		t.Errorf("filtered diff:\n%s", out.String())
	}
}

// TestCrashBetweenWritesAndState simulates a checkout killed after writing
// files but before state.json: status reports the partial work, and a forced
// checkout of @ recovers.
func TestCrashBetweenWritesAndState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, w, f := cloneTiny(t)
	conn := dial(t, s.Addr)
	tip := strings.TrimSpace(gitOut(t, s.GitDir, "rev-parse", "main"))
	older := strings.TrimSpace(gitOut(t, s.GitDir, "rev-parse", "main~3"))
	for _, tc := range []struct {
		name         string
		keepDirstate bool // the crash also lost the dirstate write
	}{{"state and dirstate lost", false}, {"state lost", true}} {
		saved := map[string][]byte{}
		for _, name := range []string{"state.json", "dirstate"} {
			b, err := os.ReadFile(w.Meta(name))
			if err != nil {
				t.Fatal(err)
			}
			saved[name] = b
		}
		if _, err := w.Checkout(ctx, f, resolve(t, conn, older), false); err != nil {
			t.Fatal(err)
		}
		restore := []string{"state.json"}
		if !tc.keepDirstate {
			restore = append(restore, "dirstate")
		}
		for _, name := range restore {
			if err := os.WriteFile(w.Meta(name), saved[name], 0o600); err != nil {
				t.Fatal(err)
			}
		}
		crashed, err := client.Open(w.Root)
		if err != nil {
			t.Fatal(err)
		}
		changed := strings.Fields(gitOut(t, s.GitDir, "diff", "--name-only", "--no-renames", tip, older))
		got := statusOf(t, crashed, f)
		if len(got) == 0 || len(got) < len(changed) {
			t.Fatalf("%s: status after crash %q, git changes %q", tc.name, got, changed)
		}
		if _, err := crashed.Checkout(ctx, f, resolve(t, conn, tip), true); err != nil {
			t.Fatalf("%s: forced checkout: %v", tc.name, err)
		}
		// Files the interrupted checkout created stay, untracked (as in git).
		for _, st := range statusOf(t, crashed, f) {
			p, ok := strings.CutPrefix(st, "? ")
			if !ok || !slices.Contains(changed, p) {
				t.Errorf("%s: status after recovery has %q", tc.name, st)
				continue
			}
			if err := os.Remove(filepath.Join(crashed.Root, p)); err != nil {
				t.Fatal(err)
			}
		}
		assertMatchesGit(t, crashed.Root, s, tip, nil)
		w = crashed
	}
}

// TestStatusFast: status on an unchanged branchy checkout takes < 200 ms
// once the first run has refreshed the dirstate (M5 acceptance).
func TestStatusFast(t *testing.T) {
	t.Parallel()
	s := servertest.Start(t, "branchy")
	dir := filepath.Join(t.TempDir(), "ws")
	res, err := client.Clone(context.Background(), s.Addr, dir, client.CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w := res.Workspace
	f := client.NewFetcher(dial(t, s.Addr), "default", client.NewObjectCache(w.Meta("objects"), 0))
	if got := statusOf(t, w, f); len(got) != 0 {
		t.Fatalf("status %q", got[:min(5, len(got))])
	}
	best := time.Hour
	for range 3 {
		start := time.Now()
		statusOf(t, w, f)
		best = min(best, time.Since(start))
	}
	t.Logf("status on branchy (%d files): %v", res.Files, best)
	if best > 200*time.Millisecond {
		t.Errorf("status took %v, want < 200 ms", best)
	}
}
