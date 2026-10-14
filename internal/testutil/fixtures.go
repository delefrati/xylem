package testutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/hash"
)

// RepoRoot returns the module root (where go.mod is).
func RepoRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate testutil source")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// fixtureCacheDir is <user cache>/xylem-fixtures/<hash of the generators>, so
// fixtures are generated once and any generator change yields a new key.
func fixtureCacheDir(t testing.TB) string {
	t.Helper()
	root := RepoRoot(t)
	var src []byte
	for _, p := range []string{"scripts/make-fixture-repo.sh", "scripts/genrepo/main.go"} {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		src = append(append(src, b...), 0)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "xylem-fixtures", hash.Sum(nil, src).Short(12))
}

// GitFixture returns a git repository for profile "tiny", "edge" or
// "branchy" (10-testing.md §Fixture repositories). Fixtures are cached across
// test runs (see fixtureCacheDir) and created atomically, so concurrent test
// binaries can share them. Tests must not modify the result; use CopyFixture.
// It skips the test if git is not installed.
func GitFixture(t testing.TB, profile string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := filepath.Join(fixtureCacheDir(t), profile)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return dir
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		t.Fatal(err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), profile+".tmp-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	repoRoot := RepoRoot(t)
	var cmd *exec.Cmd
	switch profile {
	case "tiny", "edge":
		cmd = exec.Command(filepath.Join(repoRoot, "scripts", "make-fixture-repo.sh"), tmp, profile) //nolint:gosec // G204: our own fixture script
	case "branchy":
		cmd = exec.Command("go", "run", "./scripts/genrepo", tmp) //nolint:gosec // G204: our own fixture generator
		cmd.Dir = repoRoot
	default:
		t.Fatalf("unknown fixture profile %q", profile)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating %s fixture: %v\n%s", profile, err, out)
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another test binary finished first; use its copy.
		if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr != nil {
			t.Fatalf("installing %s fixture: %v", profile, errors.Join(err, statErr))
		}
	}
	return dir
}

// CopyFixture returns a private copy of a fixture that the test may modify
// (e.g. to add commits).
func CopyFixture(t testing.TB, profile string) string {
	t.Helper()
	src := GitFixture(t, profile)
	dst := filepath.Join(t.TempDir(), profile)
	Git(t, "", "clone", "-q", "--no-local", src, dst)
	return dst
}

// Git runs git in dir (the user's git config ignored) and returns its trimmed
// standard output, failing the test on error.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // G204: tests drive git with computed arguments (ADR-010)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}
