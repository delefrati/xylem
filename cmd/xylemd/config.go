package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Defaults from 11-dev-environment.md §Server configuration.
const defaultDBURL = "postgres://xylem:xylem@localhost:54329/xylem?sslmode=disable" //nolint:gosec // G101: documented dev-only default (11-dev-environment.md)

// envOr returns the value of the environment variable key, or def if unset.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// addDBFlag registers --db with its XYLEM_DB_URL fallback.
func addDBFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "db", envOr("XYLEM_DB_URL", defaultDBURL), "Postgres URL (env XYLEM_DB_URL)")
}

// newLogger returns a text logger on stderr for admin commands.
func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		return nil, fmt.Errorf("--log-level %q: %w", level, err)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}
