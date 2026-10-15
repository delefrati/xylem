//go:build integration

package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/server/servertest"
	"github.com/delefrati/xylem/internal/testutil"
)

func TestQueryCommands(t *testing.T) {
	t.Parallel()
	s := servertest.Start(t, "tiny")
	git := func(args ...string) string { return testutil.GitRaw(t, s.GitDir, args...) }
	dir := filepath.Join(t.TempDir(), "ws")
	if _, err := runXY("clone", s.Addr, dir); err != nil {
		t.Fatal(err)
	}
	xy := func(args ...string) string {
		t.Helper()
		out, err := runXY(append([]string{"-C", dir}, args...)...)
		if err != nil {
			t.Fatalf("xy %q: %v\n%s", args, err, out)
		}
		return out
	}

	t.Run("log", func(t *testing.T) {
		if got, want := xy("log", "--oneline", "-n", "3"), git("log", "--first-parent", "--format=%s", "-n3", "main"); !sameSubjects(got, want) {
			t.Errorf("log --oneline:\n%s\ngit:\n%s", got, want)
		}
		var cs []client.ChangesetInfo
		if err := json.Unmarshal([]byte(xy("log", "--json")), &cs); err != nil {
			t.Fatal(err)
		}
		if got, want := gitSHAs(cs), strings.Fields(git("rev-list", "--first-parent", "main")); !slices.Equal(got, want) {
			t.Errorf("log --json shas %q, git %q", got, want)
		}
		if err := json.Unmarshal([]byte(xy("log", "--json", "docs")), &cs); err != nil {
			t.Fatal(err)
		}
		if got, want := gitSHAs(cs), strings.Fields(git("log", "--first-parent", "--format=%H", "main", "--", "docs")); !slices.Equal(got, want) {
			t.Errorf("log docs %q, git %q", got, want)
		}
		if out := xy("log", "-n", "1"); !strings.Contains(out, "Author: Ada Lovelace <ada@example.com>\nDate:   Tue Nov 14 22:23:20 2023 +0000\n\n    final\n") {
			t.Errorf("log -n 1:\n%s", out)
		}
	})

	t.Run("show", func(t *testing.T) {
		sha := strings.TrimSpace(git("rev-parse", "main~2"))
		out := xy("show", "git:"+sha)
		if !strings.Contains(out, "Git:    "+sha+"\n") {
			t.Errorf("show header:\n%s", out)
		}
		if got, want := diffBody(out), diffBody(git("show", "--format=", sha)); got != want {
			t.Errorf("show diff:\n%s\ngit:\n%s", got, want)
		}
		var shown struct {
			Changeset client.ChangesetInfo `json:"changeset"`
			Files     []struct {
				Path   client.Path `json:"path"`
				Status string      `json:"status"`
			} `json:"files"`
		}
		if err := json.Unmarshal([]byte(xy("show", "--json")), &shown); err != nil { // @
			t.Fatal(err)
		}
		if shown.Changeset.ID != s.TipID.String() || len(shown.Files) != 1 || string(shown.Files[0].Path) != "README" || shown.Files[0].Status != "modified" {
			t.Errorf("show --json: %+v", shown)
		}
	})

	t.Run("blame", func(t *testing.T) {
		out := xy("blame", "README")
		gitLines := strings.Split(strings.TrimSuffix(git("blame", "--first-parent", "-l", "-s", "main", "--", "README"), "\n"), "\n")
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(lines) != len(gitLines) || !strings.Contains(lines[0], "(Ada Lovelace 2023-11-14 1) ") {
			t.Fatalf("blame:\n%s\ngit:\n%s", out, strings.Join(gitLines, "\n"))
		}
		var res struct {
			Lines []struct {
				Origin  string      `json:"origin"`
				Content client.Path `json:"content"`
			} `json:"lines"`
			Origins map[string]client.ChangesetInfo `json:"origins"`
		}
		if err := json.Unmarshal([]byte(xy("blame", "--json", "README")), &res); err != nil {
			t.Fatal(err)
		}
		for i, l := range res.Lines {
			gitSHA, content, _ := strings.Cut(gitLines[i], " ")
			_, content, _ = strings.Cut(content, ") ")
			if res.Origins[l.Origin].GitSHA != gitSHA || string(l.Content) != content {
				t.Errorf("line %d: %s %q, git %s %q", i+1, res.Origins[l.Origin].GitSHA, l.Content, gitSHA, content)
			}
		}
	})

	t.Run("cat", func(t *testing.T) {
		if got, want := xy("cat", "README"), git("show", "main:README"); got != want {
			t.Errorf("cat README = %q, git %q", got, want)
		}
		if got, want := xy("cat", "-r", "@~1", "README"), git("show", "main~1:README"); got != want {
			t.Errorf("cat -r @~1 = %q, git %q", got, want)
		}
		// From a subdirectory, paths are relative to it.
		if got, want := runOK(t, "-C", filepath.Join(dir, "src"), "cat", "main.go"), git("show", "main:src/main.go"); got != want {
			t.Errorf("cat in src = %q, git %q", got, want)
		}
		// Without a workspace.
		outside := t.TempDir()
		if got, want := runOK(t, "-C", outside, "--server", s.Addr, "cat", "-r", "main~2", "README"), git("show", "main~2:README"); got != want {
			t.Errorf("cat outside a workspace = %q, git %q", got, want)
		}
		if _, err := runXY("-C", outside, "--server", s.Addr, "cat", "-r", "@", "README"); client.ExitCode(err) != client.ExitUsage {
			t.Errorf("@ outside a workspace: %v", err)
		}
		if _, err := runXY("-C", outside, "cat", "README"); err == nil || !strings.Contains(err.Error(), "--server") {
			t.Errorf("no server: %v", err)
		}
	})

	t.Run("ls", func(t *testing.T) {
		out := xy("ls")
		var names []string
		for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
			_, name, _ := strings.Cut(line, "\t")
			names = append(names, name)
		}
		if want := strings.Fields(git("ls-tree", "--name-only", "main")); !slices.Equal(names, want) {
			t.Errorf("ls = %q, git %q\n%s", names, want, out)
		}
		if !strings.HasPrefix(out, "100644 blob ") || !strings.Contains(out, "040000 tree ") {
			t.Errorf("ls format:\n%s", out)
		}
		var entries []struct {
			Name client.Path `json:"name"`
		}
		if err := json.Unmarshal([]byte(runOK(t, "-C", filepath.Join(dir, "src"), "ls", "-R", "--json")), &entries); err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || string(entries[0].Name) != "src/main.go" {
			t.Errorf("ls -R in src: %q", entries)
		}
	})
}

func runOK(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runXY(args...)
	if err != nil {
		t.Fatalf("xy %q: %v\n%s", args, err, out)
	}
	return out
}

func sameSubjects(oneline, subjects string) bool {
	a := strings.Split(strings.TrimSuffix(oneline, "\n"), "\n")
	b := strings.Split(strings.TrimSuffix(subjects, "\n"), "\n")
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if _, subject, _ := strings.Cut(a[i], " "); subject != b[i] {
			return false
		}
	}
	return true
}

func gitSHAs(cs []client.ChangesetInfo) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.GitSHA
	}
	return out
}

// diffBody keeps a diff's ---/+++ and hunk lines (git adds "index" lines and
// says "diff --git"; Xylem says "diff --xy").
func diffBody(s string) string {
	var b strings.Builder
	in := false
	for _, line := range strings.SplitAfter(s, "\n") {
		if strings.HasPrefix(line, "diff --") {
			in = true
			continue
		}
		if in && !strings.HasPrefix(line, "index ") {
			b.WriteString(line)
		}
	}
	return b.String()
}
