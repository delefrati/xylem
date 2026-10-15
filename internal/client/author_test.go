package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAuthor(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("HOME", t.TempDir())
	gitcfg := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", gitcfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XYLEM_AUTHOR", "")

	if _, err := ResolveAuthor(); err != ErrNoAuthor { //nolint:errorlint // sentinel returned as is
		t.Fatalf("nothing configured: %v", err)
	}
	if err := os.WriteFile(gitcfg, []byte("[user]\n\tname = Git User\n\temail = git@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a, err := ResolveAuthor(); err != nil || a != "Git User <git@example.com>" {
		t.Errorf("git config: %q, %v", a, err)
	}
	if err := os.MkdirAll(filepath.Join(cfgHome, "xylem"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgHome, "xylem", "config.toml"), []byte("[user]\nname = \"Xy User\"\nemail = \"xy@example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a, err := ResolveAuthor(); err != nil || a != "Xy User <xy@example.com>" {
		t.Errorf("user config: %q, %v", a, err)
	}
	t.Setenv("XYLEM_AUTHOR", "Env User <env@example.com>")
	if a, err := ResolveAuthor(); err != nil || a != "Env User <env@example.com>" {
		t.Errorf("env: %q, %v", a, err)
	}
	t.Setenv("XYLEM_AUTHOR", "no email")
	if _, err := ResolveAuthor(); err == nil {
		t.Error("malformed XYLEM_AUTHOR accepted")
	}
}
