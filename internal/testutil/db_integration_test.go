//go:build integration

package testutil_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/delefrati/xylem/internal/testutil"
)

func TestNewDBIsolatedAndMigrated(t *testing.T) {
	t.Parallel()
	for i := range 20 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			pool := testutil.NewDB(t)
			elapsed := time.Since(start)
			ctx := t.Context()
			var version int
			if err := pool.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version < 1 {
				t.Fatalf("schema_migrations: %d, %v", version, err)
			}
			if _, err := pool.Exec(ctx, "INSERT INTO repos (name) VALUES ($1)", fmt.Sprint("repo-", i)); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM repos").Scan(&n); err != nil || n != 1 {
				t.Errorf("repos in this test's database = %d, %v; want 1 (isolation)", n, err)
			}
			t.Logf("NewDB took %v", elapsed)
		})
	}
}
