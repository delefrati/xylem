package diff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitU0 returns the hunk headers of `git diff --no-index -U0` without the
// indent heuristic (compaction only), or skips the test without git.
func gitU0(t *testing.T, a, b string) []string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	pa, pb := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(pa, []byte(a), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pb, []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-c", "diff.indentHeuristic=false", "diff", "--no-index", "--no-color", "-U0", pa, pb) //nolint:gosec // G204: test compares with git
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, _ := cmd.Output() // exit 1 means "differs"
	var headers []string
	for _, l := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(l, "@@ "); ok {
			ranges, _, _ := strings.Cut(rest, " @@") // drop git's function context
			headers = append(headers, "@@ "+ranges+" @@")
		}
	}
	return headers
}

func ourU0(a, b string) []string {
	var headers []string
	for _, h := range Hunks([]byte(a), []byte(b), 0) {
		headers = append(headers, h.Header())
	}
	return headers
}

// TestCompactionMatchesGit: where several minimal scripts exist, runs of
// changed lines land where git's compaction puts them.
func TestCompactionMatchesGit(t *testing.T) {
	t.Parallel()
	// Inputs where Myers alone picks a different (equally minimal) script
	// than git; compaction moves the runs to git's positions.
	cases := [][2]string{
		{"b\n\n}\nb\n}\nb\nb\n", "\n\nb\nb\na\na\nb\n"},
		{"\nb\na\na\n}\n", "a\n}\n}\na\na\na\nb\n"},
		{"}\n\nb\nb\n\n\na\n\n", "\na\n"},
		{"\n\na\nb\n", "}\n\n"},
		{"}\nb\n\nb\nb\n", "a\na\n\nb\na\nb\n\nb\na\n}\nb\n"},
		{"a\nb\nb\na\n\na\na\n\nb\n\n", "\na\n}\nb\nb\n\nb\n}\n"},
	}
	for _, c := range cases {
		want, got := gitU0(t, c[0], c[1]), ourU0(c[0], c[1])
		if strings.Join(want, "|") != strings.Join(got, "|") {
			t.Errorf("%q → %q: hunks %q, git %q", c[0], c[1], got, want)
		}
	}
}
