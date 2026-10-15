//go:build e2e

// Package e2e runs the real xy binary against an in-process server
// (M5.11): clone, modify, status, diff, checkout and sparse changes.
package e2e

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/delefrati/xylem/internal/server/servertest"
	"github.com/delefrati/xylem/internal/testutil"
)

var (
	buildOnce sync.Once
	xyBin     string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if xyBin != "" {
		_ = os.RemoveAll(filepath.Dir(xyBin))
	}
	os.Exit(code)
}

// xyBinary builds cmd/xy once per test binary.
func xyBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "xylem-e2e-")
		if err != nil {
			buildErr = err
			return
		}
		xyBin = filepath.Join(dir, "xy")
		cmd := exec.Command("go", "build", "-o", xyBin, "./cmd/xy") //nolint:gosec // G204: builds the binary under test
		cmd.Dir = testutil.RepoRoot(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = errors.New("go build ./cmd/xy: " + err.Error() + "\n" + string(out))
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return xyBin
}

// xy runs the binary in dir and returns stdout and the exit code.
func xy(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(xyBinary(t), args...) //nolint:gosec // G204: the test drives its own binary
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return stdout.String() + stderr.String(), exit.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	return stdout.String(), 0
}

func ok(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, code := xy(t, dir, args...)
	if code != 0 {
		t.Fatalf("xy %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflow(t *testing.T) {
	t.Parallel()
	s := servertest.Start(t, "tiny")
	git := func(args ...string) string { return testutil.GitRaw(t, s.GitDir, args...) }
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")

	if out := ok(t, tmp, "clone", s.Addr, ws, "--json"); !strings.Contains(out, `"files": 4`) {
		t.Fatalf("clone: %s", out)
	}
	if out := ok(t, ws, "status"); out != "" {
		t.Fatalf("status after clone: %q", out)
	}

	// Modify, delete and create files.
	write(t, filepath.Join(ws, "README"), read(t, filepath.Join(ws, "README"))+"edited\n")
	if err := os.Remove(filepath.Join(ws, "TODO")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, "new.txt"), "new\n")
	if out := ok(t, ws, "status"); out != "M README\nD TODO\n? new.txt\n" {
		t.Errorf("status:\n%s", out)
	}
	if out := ok(t, filepath.Join(ws, "src"), "status"); out != "M ../README\nD ../TODO\n? ../new.txt\n" {
		t.Errorf("status in src:\n%s", out)
	}
	diff := ok(t, ws, "diff")
	for _, want := range []string{"--- a/README\n+++ b/README\n", "+edited\n", "deleted file mode 100644\n--- a/TODO\n+++ /dev/null\n"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff lacks %q:\n%s", want, diff)
		}
	}

	// Checkout another revision: refused while dirty, then forced.
	if out, code := xy(t, ws, "checkout", "main~3"); code != 4 || !strings.Contains(out, "README") {
		t.Errorf("dirty checkout: exit %d\n%s", code, out)
	}
	ok(t, ws, "checkout", "--force", "main~3")
	if read(t, filepath.Join(ws, "README")) != git("show", "main~3:README") {
		t.Error("README after checkout main~3")
	}
	if out := ok(t, ws, "status"); out != "? new.txt\n" {
		t.Errorf("status after forced checkout: %q", out)
	}
	if out := ok(t, ws, "log", "--oneline", "-n", "1"); !strings.HasSuffix(out, " "+strings.TrimSpace(git("log", "-1", "--format=%s", "main~3"))+"\n") {
		t.Errorf("log at @: %q", out)
	}
	ok(t, ws, "checkout", "main")
	if read(t, filepath.Join(ws, "README")) != git("show", "main:README") {
		t.Error("README after checkout main")
	}

	// Sparse: clone one directory, add another, remove the first.
	sp := filepath.Join(tmp, "sparse")
	ok(t, tmp, "clone", s.Addr, sp, "--sparse", "docs")
	if _, err := os.Stat(filepath.Join(sp, "src")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("src in a docs-only clone: %v", err)
	}
	ok(t, sp, "sparse", "add", "src")
	if read(t, filepath.Join(sp, "src", "main.go")) != git("show", "main:src/main.go") {
		t.Error("src/main.go after sparse add")
	}
	ok(t, sp, "sparse", "remove", "docs")
	if _, err := os.Stat(filepath.Join(sp, "docs")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("docs after sparse remove: %v", err)
	}
	if out := ok(t, sp, "sparse", "list"); out != "src\n" {
		t.Errorf("sparse list: %q", out)
	}
	if out := ok(t, sp, "status"); out != "" {
		t.Errorf("status after sparse changes: %q", out)
	}

	// Queries without a workspace.
	if got := ok(t, tmp, "--server", s.Addr, "cat", "-r", "main~1", "README"); got != git("show", "main~1:README") {
		t.Errorf("cat outside a workspace: %q", got)
	}
	if out, code := xy(t, tmp, "status"); code != 1 || !strings.Contains(out, "not inside a xylem workspace") {
		t.Errorf("status outside a workspace: exit %d\n%s", code, out)
	}
	if _, code := xy(t, ws, "checkout"); code != 2 {
		t.Errorf("checkout without a revision: exit %d", code)
	}
}
