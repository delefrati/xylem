//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/auth"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/server/servertest"
)

// TestAuth: xy sends XYLEM_TOKEN or the workspace's token; a read-only token
// can clone but not land.
func TestAuth(t *testing.T) {
	t.Parallel()
	tokens, err := auth.ParseTokens([]byte(`
[[token]]
identity = "Rita Reader <rita@example.com>"
roles = ["read"]
token = "reader-token-0123456789"

[[token]]
identity = "Walt Writer <walt@example.com>"
roles = ["read", "write"]
token = "writer-token-0123456789"
`))
	if err != nil {
		t.Fatal(err)
	}
	s := servertest.Start(t, "tiny", func(cfg *server.Config) { cfg.Auth = tokens })
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	reader := []string{"XYLEM_TOKEN=reader-token-0123456789"}

	if out, code := xy(t, tmp, "clone", s.Addr, ws); code == 0 || !strings.Contains(out, "missing token") {
		t.Fatalf("clone without a token: exit %d\n%s", code, out)
	}
	if out, code := xyEnv(t, tmp, reader, "clone", s.Addr, ws); code != 0 {
		t.Fatalf("clone as reader: exit %d\n%s", code, out)
	}
	write(t, filepath.Join(ws, "README"), "changed by the e2e auth test\n")
	if out, code := xyEnv(t, ws, reader, "commit", "-m", "try"); code == 0 || !strings.Contains(out, "needs the write role") {
		t.Fatalf("commit as reader: exit %d\n%s", code, out)
	}

	// The token in .xylem/config.toml is used when XYLEM_TOKEN is not set.
	cfgPath := filepath.Join(ws, ".xylem", "config.toml")
	cfg := read(t, cfgPath)
	withToken := strings.Replace(cfg, `token = ""`, `token = "writer-token-0123456789"`, 1)
	if withToken == cfg {
		t.Fatalf("no empty token line in config.toml:\n%s", cfg)
	}
	if err := os.WriteFile(cfgPath, []byte(withToken), 0o600); err != nil {
		t.Fatal(err)
	}
	ok(t, ws, "commit", "-m", "as writer")
	ok(t, ws, "land")
	if out := ok(t, ws, "log", "-n", "1"); !strings.Contains(out, "as writer") {
		t.Errorf("log after land:\n%s", out)
	}
	if out := ok(t, ws, "show", "@", "--json"); !strings.Contains(out, "Walt Writer") {
		t.Errorf("committer is not the token's identity:\n%s", out)
	}
}
