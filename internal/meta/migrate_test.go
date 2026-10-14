package meta_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/migrations"
)

func TestLoadMigrations(t *testing.T) {
	t.Parallel()
	ms, err := meta.LoadMigrations(fstest.MapFS{
		"0002_more.sql": {Data: []byte("two")},
		"0001_init.sql": {Data: []byte("one")},
		"README.md":     {Data: []byte("ignored")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Version != 1 || ms[1].Version != 2 || ms[1].SQL != "two" {
		t.Errorf("LoadMigrations = %+v", ms)
	}

	bad := map[string]fstest.MapFS{
		"empty":     {},
		"bad name":  {"1_init.sql": {}},
		"uppercase": {"0001_Init.sql": {}},
		"gap":       {"0001_a.sql": {}, "0003_c.sql": {}},
		"duplicate": {"0001_a.sql": {}, "0001_b.sql": {}},
		"no first":  {"0002_b.sql": {}},
	}
	for name, fsys := range bad {
		if _, err := meta.LoadMigrations(fsys); err == nil {
			t.Errorf("%s: LoadMigrations succeeded", name)
		}
	}
}

func TestEmbeddedMigrations(t *testing.T) {
	t.Parallel()
	ms, err := meta.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if ms[0].Name != "0001_init.sql" || !strings.Contains(ms[0].SQL, "CREATE TABLE changesets") {
		t.Errorf("unexpected first migration %s", ms[0].Name)
	}
}

func TestFingerprint(t *testing.T) {
	t.Parallel()
	a := []meta.Migration{{Version: 1, Name: "0001_a.sql", SQL: "x"}}
	b := []meta.Migration{{Version: 1, Name: "0001_a.sql", SQL: "y"}}
	c := []meta.Migration{{Version: 1, Name: "0001_b.sql", SQL: "x"}}
	if fa := meta.Fingerprint(a); len(fa) != 12 || fa == meta.Fingerprint(b) || fa == meta.Fingerprint(c) {
		t.Errorf("Fingerprint not sensitive to content and names: %s %s %s", fa, meta.Fingerprint(b), meta.Fingerprint(c))
	}
	sameAsA := []meta.Migration{{Version: 1, Name: "0001_a.sql", SQL: "x"}}
	if meta.Fingerprint(a) != meta.Fingerprint(sameAsA) {
		t.Error("Fingerprint differs for identical migrations")
	}
}
