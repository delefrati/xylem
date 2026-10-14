//go:build integration

package importer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/importer"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/testutil"
)

// gitSHAOf finds the git sha mapped to a changeset id.
func gitSHAOf(t *testing.T, r *repo.Repo, id hash.ID) gitsource.SHA {
	t.Helper()
	m, err := r.GitSHAMap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for sha, cs := range m {
		if cs == id {
			return sha
		}
	}
	t.Fatalf("changeset %s has no git sha", id.Short(12))
	return gitsource.SHA{}
}

func TestImportCheckpoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := testutil.GitFixture(t, "branchy")
	src := openGit(t, dir)
	r := repotest.New(t)
	opts := importer.Options{Batch: 100, Limit: 700}
	stats := runImport(t, r, src, opts)
	if stats.Commits != 700 {
		t.Fatalf("limited import created %d changesets, want 700", stats.Commits)
	}
	source := "file://" + dir // the default Source key
	last, ok, err := r.ImportState(ctx, source, "main")
	if err != nil || !ok {
		t.Fatalf("import state: %v, %v", ok, err)
	}
	shaMap, _ := r.GitSHAMap(ctx)
	if _, imported := shaMap[last]; !imported || len(shaMap) != 700 {
		t.Errorf("import state %s imported=%v, %d commits mapped", gitsource.SHA(last), imported, len(shaMap))
	}

	main, err := r.GetRef(ctx, repo.MainRef)
	if err != nil {
		t.Fatal(err)
	}
	if main.Version < 5 {
		t.Errorf("main moved %d times; want a fast-forward at most batch boundaries", main.Version)
	}
	chain, err := src.FirstParentChain(ctx, stats.Tip)
	if err != nil {
		t.Fatal(err)
	}
	if !chain[gitSHAOf(t, r, main.ID)] {
		t.Error("main points at a commit off the branch's first-parent chain")
	}

	// Finish the import.
	rest := runImport(t, r, src, importer.Options{Batch: 100})
	total, _ := strconv.Atoi(testutil.Git(t, dir, "rev-list", "--count", "main"))
	if int(stats.Commits+rest.Commits) != total || rest.Existing != 0 {
		t.Errorf("700 + %d changesets (%d existing), git has %d commits", rest.Commits, rest.Existing, total)
	}
	if main, _ := r.GetRef(ctx, repo.MainRef); gitSHAOf(t, r, main.ID) != rest.Tip {
		t.Error("main is not at the branch tip after finishing")
	}
}

func TestImportKillAndResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := testutil.GitFixture(t, "branchy")
	src := openGit(t, dir)
	r := repotest.New(t)
	total, _ := strconv.Atoi(testutil.Git(t, dir, "rev-list", "--count", "main"))

	im, err := importer.New(r, src, importer.Options{Batch: 50})
	if err != nil {
		t.Fatal(err)
	}
	killCtx, kill := context.WithCancel(ctx)
	go func() { // cancel once some commits are in
		for im.Stats().Commits < 400 {
			time.Sleep(time.Millisecond)
		}
		kill()
	}()
	first, err := im.Run(killCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("killed import returned %v, want context.Canceled", err)
	}
	if first.Commits < 400 || int(first.Commits) >= total {
		t.Fatalf("killed import created %d of %d changesets", first.Commits, total)
	}
	// The checkpoint ran on the way out.
	if last, ok, _ := r.ImportState(ctx, "file://"+dir, "main"); !ok || last == ([20]byte{}) {
		t.Error("no import state recorded after the kill")
	}

	second := runImport(t, r, src, importer.Options{Batch: 50})
	shaMap, _ := r.GitSHAMap(ctx)
	if len(shaMap) != total || int(first.Commits+second.Commits) != total || second.Existing != 0 {
		t.Errorf("after resume: %d mapped, %d + %d created (%d existing), want %d total",
			len(shaMap), first.Commits, second.Commits, second.Existing, total)
	}
	if second.Reused == 0 {
		t.Error("resume re-read every blob from git; expected git_blobs or cache hits")
	}
	main, _ := r.GetRef(ctx, repo.MainRef)
	if gitSHAOf(t, r, main.ID) != second.Tip {
		t.Error("main is not at the branch tip after resuming")
	}
	if report, err := r.Verify(ctx, repo.VerifyOptions{Objects: true}); err != nil || len(report.Violations) != 0 {
		t.Fatalf("verify after resume: %v %v", report.Violations, err)
	}
	if report, err := importer.VerifyImport(ctx, r, src, importer.VerifyOptions{}); err != nil || len(report.Violations) != 0 || report.Commits != total {
		t.Fatalf("verify-import after resume: %d commits, %v %v", report.Commits, report.Violations, err)
	}
}

func TestImportStaleStateResumes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := testutil.GitFixture(t, "edge")
	src := openGit(t, dir)
	r := repotest.New(t)
	stats := runImport(t, r, src, importer.Options{Limit: 20})
	// Pretend the checkpoint after commit 20 was lost: state points much earlier.
	var early gitsource.SHA
	for sha, err := range src.RevList(ctx, stats.Tip) {
		if err != nil {
			t.Fatal(err)
		}
		early = sha
		break
	}
	if err := r.SetImportState(ctx, "file://"+dir, "main", early); err != nil {
		t.Fatal(err)
	}
	rest := runImport(t, r, src, importer.Options{})
	total, _ := strconv.Atoi(testutil.Git(t, dir, "rev-list", "--count", "main"))
	if int(stats.Commits+rest.Commits) != total || rest.Existing != 0 {
		t.Errorf("stale state: 20 + %d created (%d existing), want %d", rest.Commits, rest.Existing, total)
	}
}

func TestImportIncremental(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := testutil.CopyFixture(t, "edge")
	src := openGit(t, dir)
	r := repotest.New(t)
	first := runImport(t, r, src, importer.Options{})
	oldTip := first.Tip

	// New history: edits, a delete, a symlink, an exec file, and a feature
	// branch merged with a merge commit.
	g := func(args ...string) { testutil.Git(t, dir, args...) }
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("README", "changed after the first import\n")
	g("rm", "-q", "empty.txt")
	g("commit", "-q", "-am", "incremental: edit and delete")
	g("checkout", "-q", "-b", "feature")
	write("feature.txt", "feature\n")
	if err := os.Symlink("feature.txt", filepath.Join(dir, "feature-link")); err != nil {
		t.Fatal(err)
	}
	g("add", "-A")
	g("commit", "-q", "-m", "feature: add file and link")
	g("checkout", "-q", "main")
	write("tool.sh", "#!/bin/sh\n")
	g("add", "tool.sh")
	g("update-index", "--chmod=+x", "tool.sh")
	g("commit", "-q", "-m", "main: add tool")
	g("merge", "-q", "--no-ff", "--no-edit", "feature")

	second := runImport(t, r, src, importer.Options{})
	newCount, _ := strconv.Atoi(testutil.Git(t, dir, "rev-list", "--count", "main", "^"+oldTip.String()))
	if int(second.Commits) != newCount || second.Existing != 0 {
		t.Fatalf("incremental import created %d changesets (%d existing), want %d new", second.Commits, second.Existing, newCount)
	}
	report, err := importer.VerifyImport(ctx, r, src, importer.VerifyOptions{Since: &oldTip})
	if err != nil || len(report.Violations) != 0 || report.Commits != newCount {
		t.Fatalf("verify-import --since: %d commits, %v %v", report.Commits, report.Violations, err)
	}
	main, _ := r.GetRef(ctx, repo.MainRef)
	if gitSHAOf(t, r, main.ID) != second.Tip {
		t.Error("main is not at the new tip")
	}
	if rep, err := r.Verify(ctx, repo.VerifyOptions{Objects: true}); err != nil || len(rep.Violations) != 0 {
		t.Fatalf("verify: %v %v", rep.Violations, err)
	}
}
