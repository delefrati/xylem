package testutil_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/delefrati/xylem/internal/testutil"
)

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return testutil.Git(t, dir, args...)
}

// The fixtures are deterministic: these shas only change if a generator does.
func TestFixturesAreDeterministic(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"tiny":    "dff8316cc9143b8d5099cfe5be15ec622cffca8c",
		"edge":    "1eae83c0bf7b2ecd8c068d4678aee88810cfb120",
		"branchy": "f9aa238196a2b77e69e09e53df8e33098918c59d",
	}
	for profile, sha := range want {
		dir := testutil.GitFixture(t, profile)
		got := gitOut(t, dir, "rev-parse", "main")
		if sha != "" && got != sha {
			t.Errorf("%s main = %s, want %s (did a generator change?)", profile, got, sha)
		}
		t.Logf("%s: main %s, %s commits", profile, got, gitOut(t, dir, "rev-list", "--count", "main"))
	}
	first, second := testutil.GitFixture(t, "tiny"), testutil.GitFixture(t, "tiny")
	if first != second {
		t.Error("fixture not cached")
	}
}

func TestCopyFixture(t *testing.T) {
	t.Parallel()
	dir := testutil.CopyFixture(t, "tiny")
	if gitOut(t, dir, "rev-parse", "main") != gitOut(t, testutil.GitFixture(t, "tiny"), "rev-parse", "main") {
		t.Error("copy has a different main")
	}
}

// Fixture scripts run concurrently into sibling directories (cold caches in
// CI); they must not share any file outside their own directory.
func TestFixtureScriptConcurrent(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	script := filepath.Join(testutil.RepoRoot(t), "scripts", "make-fixture-repo.sh")
	var wg sync.WaitGroup
	outs := make([]string, 4)
	for i := range outs {
		wg.Go(func() {
			dir := filepath.Join(parent, fmt.Sprint("edge-", i))
			out, err := exec.Command(script, dir, "edge").CombinedOutput() //nolint:gosec // G204: our own fixture script
			if err != nil {
				t.Errorf("concurrent edge generation %d: %v\n%s", i, err, out)
				return
			}
			outs[i] = testutil.Git(t, dir, "rev-parse", "main")
		})
	}
	wg.Wait()
	for _, o := range outs[1:] {
		if o != outs[0] {
			t.Errorf("concurrent generations differ: %v", outs)
		}
	}
}
