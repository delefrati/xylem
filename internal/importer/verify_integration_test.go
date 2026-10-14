//go:build integration

package importer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/importer"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/testutil"
)

func TestVerifyImportFixtures(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"tiny", "edge", "branchy"} {
		t.Run(profile, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			src := openGit(t, testutil.GitFixture(t, profile))
			r := repotest.New(t)
			runImport(t, r, src, importer.Options{})
			report, err := importer.VerifyImport(ctx, r, src, importer.VerifyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Violations) != 0 || report.Commits == 0 {
				t.Fatalf("verify-import --all: %d commits, violations %v", report.Commits, report.Violations)
			}
			t.Logf("%s: %d commits, %d files compared", profile, report.Commits, report.Files)
			s1, err := importer.VerifyImport(ctx, r, src, importer.VerifyOptions{Sample: 5, Seed: 7})
			if err != nil || s1.Commits != min(5, report.Commits) || len(s1.Violations) != 0 {
				t.Errorf("sample: %+v, %v", s1, err)
			}
		})
	}
}

func TestVerifyImportDetectsMismatches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := openGit(t, testutil.GitFixture(t, "edge"))

	t.Run("wrong blob mapping", func(t *testing.T) {
		t.Parallel()
		env := repotest.NewEnv(t, store.NewMem())
		runImport(t, env.Repo, src, importer.Options{})
		// Point the README blob's mapping at another blob.
		if _, err := env.Pool.Exec(ctx, `UPDATE git_blobs SET blob_id = (SELECT blob_id FROM git_blobs ORDER BY git_sha LIMIT 1)
			WHERE git_sha = (SELECT git_sha FROM git_blobs ORDER BY git_sha DESC LIMIT 1)`); err != nil {
			t.Fatal(err)
		}
		report, err := importer.VerifyImport(ctx, env.Repo, src, importer.VerifyOptions{})
		if err != nil || !containsAny(report.Violations, "content differs") {
			t.Errorf("violations %v, %v", report.Violations, err)
		}
	})
	t.Run("wrong root tree", func(t *testing.T) {
		t.Parallel()
		env := repotest.NewEnv(t, store.NewMem())
		runImport(t, env.Repo, src, importer.Options{})
		// Give the last changeset its parent's tree.
		if _, err := env.Pool.Exec(ctx, `UPDATE core_derived c SET root_tree = p.root_tree
			FROM core_derived p WHERE c.fp_parent = p.changeset_id
			AND c.changeset_id = (SELECT id FROM changesets ORDER BY seq DESC LIMIT 1)`); err != nil {
			t.Fatal(err)
		}
		// A fresh Repo, so the tampered core row is read from the database.
		fresh := reopen(t, env)
		report, err := importer.VerifyImport(ctx, fresh, src, importer.VerifyOptions{})
		if err != nil || !containsAny(report.Violations, "missing in Xylem", "size", "content differs") {
			t.Errorf("violations %v, %v", report.Violations, err)
		}
	})
	t.Run("partial import", func(t *testing.T) {
		t.Parallel()
		r := repotest.New(t)
		runImport(t, r, src, importer.Options{Limit: 10})
		report, err := importer.VerifyImport(ctx, r, src, importer.VerifyOptions{})
		if err != nil || !containsAny(report.Violations, "is not imported") || !containsAny(report.Violations, "main is") {
			t.Errorf("violations %v, %v", report.Violations, err)
		}
	})
}

func containsAny(lines []string, subs ...string) bool {
	for _, l := range lines {
		for _, s := range subs {
			if strings.Contains(l, s) {
				return true
			}
		}
	}
	return false
}
