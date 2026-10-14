package meta

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/delefrati/xylem/internal/hash"
)

// migrateLockKey is the pg_advisory_lock key that serializes migrations.
const migrateLockKey int64 = 0x78796c656d2d6d67 // "xylem-mg"

// Migration is one numbered SQL file.
type Migration struct {
	Version int
	Name    string // file name, e.g. "0001_init.sql"
	SQL     string
}

var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// LoadMigrations reads NNNN_name.sql files from the root of fsys, sorted by
// version. Versions must be unique and start at 1 with no gaps.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("reading migrations: %w", err)
	}
	var ms []Migration
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration file %q: want NNNN_name.sql", e.Name())
		}
		v, _ := strconv.Atoi(m[1]) // four digits always parse
		sql, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("reading migration %s: %w", e.Name(), err)
		}
		ms = append(ms, Migration{Version: v, Name: e.Name(), SQL: string(sql)})
	}
	slices.SortFunc(ms, func(a, b Migration) int { return a.Version - b.Version })
	for i, m := range ms {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migration %s: want version %d (versions start at 1 with no gaps or duplicates)", m.Name, i+1)
		}
	}
	if len(ms) == 0 {
		return nil, errors.New("no migration files")
	}
	return ms, nil
}

// Fingerprint returns 12 hex characters identifying the exact set of
// migrations (names and contents). The test helper names template databases
// with it, so any migration change yields a fresh template.
func Fingerprint(ms []Migration) string {
	var b []byte
	for _, m := range ms {
		b = append(b, m.Name...)
		b = append(b, 0)
		b = append(b, m.SQL...)
		b = append(b, 0)
	}
	return hash.Sum(nil, b).Short(12)
}

// Migrate applies pending migrations from fsys, each in its own transaction,
// recording them in schema_migrations. Concurrent callers are serialized with
// a Postgres advisory lock. It returns the versions it applied, and fails if
// the database has a migration that fsys does not know.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, log *slog.Logger) ([]int, error) {
	ms, err := LoadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: acquiring connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return nil, fmt.Errorf("migrate: taking advisory lock: %w", err)
	}
	defer func() {
		// Use a fresh context: the caller's may be canceled, and a session lock
		// left behind would block the next migrate on this connection.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLockKey)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER     PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("migrate: creating schema_migrations: %w", err)
	}

	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("migrate: reading schema_migrations: %w", err)
	}
	applied, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, fmt.Errorf("migrate: reading schema_migrations: %w", err)
	}
	if n := len(applied); n > 0 && slices.Max(applied) > len(ms) {
		return nil, fmt.Errorf("migrate: database has migration %d but this binary only knows up to %d", slices.Max(applied), len(ms))
	}

	var done []int
	for _, m := range ms {
		if slices.Contains(applied, m.Version) {
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", m.Version)
			return err
		})
		if err != nil {
			return done, fmt.Errorf("migrate: applying %s: %w", m.Name, err)
		}
		if log != nil {
			log.Info("applied migration", "version", m.Version, "file", m.Name)
		}
		done = append(done, m.Version)
	}
	return done, nil
}
