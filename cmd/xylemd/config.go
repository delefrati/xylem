package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// Defaults from 11-dev-environment.md §Server configuration.
const (
	defaultDBURL    = "postgres://xylem:xylem@localhost:54329/xylem?sslmode=disable" //nolint:gosec // G101: documented dev-only default (11-dev-environment.md)
	defaultStoreURL = "fs://./data/objects"
)

// envOr returns the value of the environment variable key, or def if unset.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// envInt64 returns the integer environment variable key, or def if unset or invalid.
func envInt64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// addDBFlag registers --db with its XYLEM_DB_URL fallback.
func addDBFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "db", envOr("XYLEM_DB_URL", defaultDBURL), "Postgres URL (env XYLEM_DB_URL)")
}

// addStoreFlag registers --store with its XYLEM_STORE fallback.
func addStoreFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "store", envOr("XYLEM_STORE", defaultStoreURL), "object store URL: fs://dir, s3://bucket?…, mem:// (env XYLEM_STORE)")
}

// newLogger returns a text logger on stderr for admin commands.
func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		return nil, fmt.Errorf("--log-level %q: %w", level, err)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}
