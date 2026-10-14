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
