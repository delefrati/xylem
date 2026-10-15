package client

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// ErrNoAuthor reports that no author identity is configured.
var ErrNoAuthor = errors.New(`no author identity: set XYLEM_AUTHOR="Name <email>", [user] name/email in ~/.config/xylem/config.toml, or git config user.name/user.email`)

var nameEmail = regexp.MustCompile(`^[^<>\n]+ <[^<>\n]*>$`)

// ResolveAuthor finds the author identity "Name <email>" (06 §Author
// identity): the XYLEM_AUTHOR environment variable, then [user] name/email in
// the user config file (~/.config/xylem/config.toml), then git's
// user.name/user.email.
func ResolveAuthor() (string, error) {
	if a := strings.TrimSpace(os.Getenv("XYLEM_AUTHOR")); a != "" {
		if !nameEmail.MatchString(a) {
			return "", fmt.Errorf(`XYLEM_AUTHOR %q is not "Name <email>"`, a)
		}
		return a, nil
	}
	if dir, err := os.UserConfigDir(); err == nil {
		var cfg struct {
			User struct {
				Name  string `toml:"name"`
				Email string `toml:"email"`
			} `toml:"user"`
		}
		path := filepath.Join(dir, "xylem", "config.toml")
		if _, err := toml.DecodeFile(path, &cfg); err == nil && cfg.User.Name != "" {
			return format(cfg.User.Name, cfg.User.Email)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("reading %s: %w", path, err)
		}
	}
	name, email := gitConfig("user.name"), gitConfig("user.email")
	if name != "" {
		return format(name, email)
	}
	return "", ErrNoAuthor
}

func format(name, email string) (string, error) {
	a := strings.TrimSpace(name) + " <" + strings.TrimSpace(email) + ">"
	if !nameEmail.MatchString(a) {
		return "", fmt.Errorf("author %q is not \"Name <email>\"", a)
	}
	return a, nil
}

func gitConfig(key string) string {
	out, err := exec.Command("git", "config", "--get", key).Output() //nolint:gosec // G204: a fixed git config key
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
