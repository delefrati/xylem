// Package testutil provides database and object store helpers and fixture
// builders for tests.
package testutil

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/migrations"
)

// DefaultTestDBURL is the admin connection used to create test databases: the
// Postgres started by make up (and the CI service), "postgres" database.
const DefaultTestDBURL = "postgres://xylem:xylem@localhost:54329/postgres?sslmode=disable" //nolint:gosec // G101: local test Postgres from make up (10-testing.md)

// AdminURL returns XYLEM_TEST_DB_URL, or DefaultTestDBURL.
func AdminURL() string {
	if u := os.Getenv("XYLEM_TEST_DB_URL"); u != "" {
		return u
	}
	return DefaultTestDBURL
}

// withDatabase returns rawURL with its database name replaced.
func withDatabase(t testing.TB, rawURL, db string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing database url: %v", err)
	}
	u.Path = "/" + db
	return u.String()
}

func adminConn(t testing.TB, ctx context.Context) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, AdminURL())
	if err != nil {
		t.Fatalf("connecting to test Postgres (run make up?): %v", err)
	}
	return conn
}

func randomName(prefix string) string {
	var b [8]byte
	_, _ = cryptorand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// createDB creates database name (optionally from a template), opens a pool on
// it, and drops it when the test ends.
func createDB(t testing.TB, template string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name := randomName("t_")
	conn := adminConn(t, ctx)
	defer func() { _ = conn.Close(ctx) }()

	stmt := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if template != "" {
		stmt += " TEMPLATE " + pgx.Identifier{template}.Sanitize()
	}
	if _, err := conn.Exec(ctx, stmt); err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	pool, err := pgxpool.New(ctx, withDatabase(t, AdminURL(), name))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		conn := adminConn(t, ctx)
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("dropping test database %s: %v", name, err)
		}
	})
	return pool
}

// NewEmptyDB returns a pool on a fresh, empty database (no migrations), dropped
// when the test ends. Most tests want NewDB instead.
func NewEmptyDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return createDB(t, "")
}

// templateLockKey is the pg_advisory_lock key that serializes template
// creation across concurrently running test binaries.
const templateLockKey int64 = 0x78796c656d2d746d // "xylem-tm"

// NewDB returns a pool on a fresh database with all migrations applied,
// dropped when the test ends. It clones a template database named after the
// migrations' fingerprint (10-testing.md §Database tests), so it takes
// milliseconds and tests can run in parallel.
func NewDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return createDB(t, ensureTemplate(t))
}

// ensureTemplate creates xylem_tmpl_<fingerprint> if missing. It is built
// under another name and renamed when complete, so a crash never leaves a
// half-migrated template behind. Templates are never dropped.
func ensureTemplate(t testing.TB) string {
	t.Helper()
	ms, err := meta.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	name := "xylem_tmpl_" + meta.Fingerprint(ms)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn := adminConn(t, ctx)
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", templateLockKey); err != nil {
		t.Fatalf("locking template creation: %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", templateLockKey) }()

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		t.Fatalf("checking template database: %v", err)
	}
	if exists {
		return name
	}

	building := pgx.Identifier{name + "_building"}.Sanitize()
	for _, stmt := range []string{
		"DROP DATABASE IF EXISTS " + building + " WITH (FORCE)",
		"CREATE DATABASE " + building,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("creating template database: %v", err)
		}
	}
	pool, err := pgxpool.New(ctx, withDatabase(t, AdminURL(), name+"_building"))
	if err != nil {
		t.Fatalf("opening template database: %v", err)
	}
	_, err = meta.Migrate(ctx, pool, migrations.FS, nil)
	pool.Close()
	if err != nil {
		t.Fatalf("migrating template database: %v", err)
	}
	if _, err := conn.Exec(ctx, "ALTER DATABASE "+building+" RENAME TO "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("finishing template database: %v", err)
	}
	return name
}
