//go:build integration

package importer_test

import (
	"bytes"
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/importer"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/testutil"
)

func openGit(t *testing.T, dir string) *gitsource.Repo {
	t.Helper()
	src, err := gitsource.Open(context.Background(), dir, gitsource.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

func runImport(t *testing.T, r *repo.Repo, src *gitsource.Repo, opts importer.Options) importer.Stats {
	t.Helper()
	im, err := importer.New(r, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := im.Run(context.Background())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return stats
}

type fileEntry struct {
	mode object.Mode
	size uint64
	blob hash.ID
}

// walk lists every file under root.
func walk(t *testing.T, r *repo.Repo, root hash.ID, prefix string, out map[string]fileEntry) {
	t.Helper()
	tree, err := r.Trees().ReadTree(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tree.Entries {
		p := prefix + string(e.Name)
		if e.Mode == object.ModeTree {
			walk(t, r, e.ID, p+"/", out)
		} else {
			out[p] = fileEntry{e.Mode, e.Size, e.ID}
		}
	}
}

// checkCommit compares a changeset's tree with git ls-tree (submodules excluded).
func checkCommit(t *testing.T, r *repo.Repo, src *gitsource.Repo, sha gitsource.SHA, id hash.ID) {
	t.Helper()
	ctx := context.Background()
	cs, err := r.Changeset(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]fileEntry{}
	walk(t, r, cs.Core.RootTree, "", got)
	entries, err := src.LsTree(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, e := range entries {
		if e.Mode == gitsource.ModeSubmodule {
			continue
		}
		want++
		f, ok := got[string(e.Path)]
		if !ok {
			t.Fatalf("commit %s: %q missing in Xylem", sha, e.Path)
		}
		content, err := src.Blob(ctx, e.SHA)
		if err != nil {
			t.Fatal(err)
		}
		mode := map[uint32]object.Mode{gitsource.ModeFile: object.ModeFile, gitsource.ModeExec: object.ModeExec, gitsource.ModeSymlink: object.ModeSymlink}[e.Mode]
		if f.mode != mode || e.Size < 0 || f.size != uint64(e.Size) || f.blob != object.ID(object.KindBlob, content) {
			t.Fatalf("commit %s: %q differs: xylem %v/%d, git %o/%d", sha, e.Path, f.mode, f.size, e.Mode, e.Size)
		}
	}
	if len(got) != want {
		t.Fatalf("commit %s: Xylem has %d files, git %d", sha, len(got), want)
	}
}

func TestImportFixtures(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"tiny", "edge", "branchy"} {
		t.Run(profile, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := testutil.GitFixture(t, profile)
			src := openGit(t, dir)
			r := repotest.New(t)

			start := time.Now()
			stats := runImport(t, r, src, importer.Options{Batch: 100})
			elapsed := time.Since(start)
			total, _ := strconv.Atoi(testutil.Git(t, dir, "rev-list", "--count", "main"))
			if int(stats.Commits) != total {
				t.Fatalf("imported %d commits, git has %d", stats.Commits, total)
			}
			t.Logf("%s: %d commits, %d blobs (%d bytes) in %v: %.0f commits/s",
				profile, stats.Commits, stats.Blobs, stats.BlobBytes, elapsed.Round(time.Millisecond), float64(stats.Commits)/elapsed.Seconds())

			shaMap, err := r.GitSHAMap(ctx)
			if err != nil {
				t.Fatal(err)
			}
			main, err := r.GetRef(ctx, repo.MainRef)
			if err != nil || main.ID != shaMap[stats.Tip] {
				t.Fatalf("main = %s, %v; want mapped tip %s", main.ID.Short(12), err, shaMap[stats.Tip].Short(12))
			}

			// Trees: every commit for tiny and edge, a sample for branchy.
			shas := slices.Collect(func(yield func(gitsource.SHA) bool) {
				for s, err := range src.RevList(ctx, stats.Tip) {
					if err != nil || !yield(s) {
						return
					}
				}
			})
			step := 1
			if profile == "branchy" {
				step = 97
			}
			for i := 0; i < len(shas); i += step {
				checkCommit(t, r, src, shas[i], shaMap[shas[i]])
			}
			checkCommit(t, r, src, stats.Tip, shaMap[stats.Tip])

			report, err := r.Verify(ctx, repo.VerifyOptions{Objects: true})
			if err != nil || len(report.Violations) != 0 {
				t.Fatalf("verify after import: %v %v", report.Violations, err)
			}

			// Re-running is a no-op.
			start = time.Now()
			again := runImport(t, r, src, importer.Options{})
			if again.Commits != 0 || again.Blobs != 0 {
				t.Errorf("re-import created %d changesets and %d blobs", again.Commits, again.Blobs)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("no-op re-import took %v (want < 2 s)", d)
			}
		})
	}
}

func TestImportRejectsReservedPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.Git(t, dir, "init", "-q", "-b", "main")
	testutil.Git(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
	// git itself accepts a .xylem path; Xylem reserves it.
	empty := testutil.Git(t, dir, "hash-object", "-w", "/dev/null")
	testutil.Git(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+empty+",.xylem/config")
	testutil.Git(t, dir, "commit", "-q", "-m", "reserved path")
	src := openGit(t, dir)
	r := repotest.New(t)
	im, err := importer.New(r, src, importer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = im.Run(context.Background())
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte(".xylem")) {
		t.Fatalf("import of a .xylem path = %v, want an error naming the path", err)
	}
	// The root commit before it was imported and checkpointed.
	if shaMap, _ := r.GitSHAMap(context.Background()); len(shaMap) != 1 {
		t.Errorf("%d commits imported before the failure, want 1", len(shaMap))
	}
}

// reopen returns a new Repo over env's stores, with cold caches.
func reopen(t *testing.T, env *repotest.Env) *repo.Repo {
	t.Helper()
	r, err := repo.Open(context.Background(), env.Meta, env.Objects, "default", repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
