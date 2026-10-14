//go:build integration

package meta_test

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/testutil"
	"github.com/delefrati/xylem/migrations"
)

func TestMigrateFreshAndIdempotent(t *testing.T) {
	t.Parallel()
	pool := testutil.NewEmptyDB(t)
	ctx := t.Context()

	applied, err := meta.Migrate(ctx, pool, migrations.FS, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(applied, []int{1}) {
		t.Errorf("first Migrate applied %v, want [1]", applied)
	}
	again, err := meta.Migrate(ctx, pool, migrations.FS, nil)
	if err != nil || len(again) != 0 {
		t.Errorf("second Migrate = %v, %v; want nothing applied", again, err)
	}

	rows, err := pool.Query(ctx, "SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"changeset_files", "changesets", "core_derived", "derived", "import_state", "mainline", "ref_log", "refs", "repos", "schema_migrations"}
	if !slices.Equal(tables, want) {
		t.Errorf("tables = %v, want %v", tables, want)
	}
}

func TestMigrateConcurrent(t *testing.T) {
	t.Parallel()
	pool := testutil.NewEmptyDB(t)
	const n = 8
	results := make([][]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { results[i], errs[i] = meta.Migrate(t.Context(), pool, migrations.FS, nil) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, r := range results {
		total += len(r)
	}
	if total != 1 {
		t.Errorf("concurrent Migrate applied %d migrations in total, want exactly 1: %v", total, results)
	}
}

func TestMigrateAppliesNewAndRollsBackFailures(t *testing.T) {
	t.Parallel()
	pool := testutil.NewEmptyDB(t)
	ctx := t.Context()
	v1 := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (x int);")}}
	if _, err := meta.Migrate(ctx, pool, v1, nil); err != nil {
		t.Fatal(err)
	}
	// A failing migration leaves no partial table and no schema_migrations row.
	v2bad := fstest.MapFS{
		"0001_a.sql": v1["0001_a.sql"],
		"0002_b.sql": {Data: []byte("CREATE TABLE b (x int); SELECT no_such_function();")},
	}
	if _, err := meta.Migrate(ctx, pool, v2bad, nil); err == nil {
		t.Fatal("failing migration succeeded")
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('b') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Errorf("table b exists after failed migration: %v, %v", exists, err)
	}
	v2 := fstest.MapFS{"0001_a.sql": v1["0001_a.sql"], "0002_b.sql": {Data: []byte("CREATE TABLE b (x int);")}}
	if applied, err := meta.Migrate(ctx, pool, v2, nil); err != nil || !slices.Equal(applied, []int{2}) {
		t.Errorf("Migrate v2 = %v, %v; want [2]", applied, err)
	}
	// A binary that knows fewer migrations than the database refuses to run.
	if _, err := meta.Migrate(ctx, pool, v1, nil); err == nil {
		t.Error("Migrate with older migration set succeeded")
	}
}
