//go:build integration

package repo_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/treeops"
)

func lookup(t *testing.T, r *repo.Repo, root hash.ID, path string) (object.Entry, bool) {
	t.Helper()
	e, found, err := treeops.Lookup(context.Background(), r.Trees(), root, []byte(path))
	if err != nil {
		t.Fatal(err)
	}
	return e, found
}

func TestCreateRootChildAndMerge(t *testing.T) {
	t.Parallel()
	r := repotest.New(t)
	readme := repotest.Put(t, r, "README", "hello\n")
	root := repotest.Commit(t, r, nil, "root\n\nbody", readme, repotest.Put(t, r, "src/main.go", "package main\n"))
	if root.Core.Gen != 1 || root.Core.FPDepth != 0 || root.Core.FPJump != root.ID || !root.Core.FPParent.IsZero() {
		t.Errorf("root core = %+v", root.Core)
	}
	if string(root.Committer.NameEmail) != string(repotest.Author.NameEmail) || root.Committer.Time != repotest.Clock().Unix() || root.Committer.TZMinutes != 0 {
		t.Errorf("default committer = %+v", root.Committer)
	}
	if root.Summary() != "root" {
		t.Errorf("summary = %q", root.Summary())
	}
	if e, ok := lookup(t, r, root.Core.RootTree, "src/main.go"); !ok || e.Size != 13 {
		t.Errorf("src/main.go in root tree: %+v %v", e, ok)
	}

	a := repotest.Commit(t, r, []hash.ID{root.ID}, "a", repotest.Put(t, r, "README", "hello a\n"))
	b := repotest.Commit(t, r, []hash.ID{root.ID}, "b", repotest.Delete("src/main.go"), repotest.Put(t, r, "lib/b", "b"))
	if a.Seq <= root.Seq || b.Seq <= a.Seq {
		t.Errorf("seqs not increasing: %d %d %d", root.Seq, a.Seq, b.Seq)
	}
	// Merge b into a: changes are relative to the first parent (a).
	merge := repotest.Commit(t, r, []hash.ID{a.ID, b.ID}, "merge", repotest.Delete("src/main.go"), repotest.Put(t, r, "lib/b", "b"))
	if merge.Core.Gen != 3 || merge.Core.FPDepth != 2 || merge.Core.FPParent != a.ID {
		t.Errorf("merge core = %+v", merge.Core)
	}
	if e, _ := lookup(t, r, merge.Core.RootTree, "README"); e.ID != object.ID(object.KindBlob, []byte("hello a\n")) {
		t.Error("merge lost the first parent's README")
	}
	if _, ok := lookup(t, r, merge.Core.RootTree, "src"); ok {
		t.Error("src should be pruned after deleting its only file")
	}
	// An empty merge is allowed.
	repotest.Commit(t, r, []hash.ID{merge.ID, b.ID}, "empty merge")
}

func TestCreateIdempotentAndConcurrent(t *testing.T) {
	t.Parallel()
	r := repotest.New(t)
	root := repotest.Commit(t, r, nil, "root", repotest.Put(t, r, "f", "1"))
	req := repo.CreateRequest{
		Parents: []hash.ID{root.ID}, Author: repotest.Author, Message: []byte("same"),
		Changes: []changeset.Change{repotest.Put(t, r, "f", "2"), repotest.Put(t, r, "g/h", "3")},
	}

	const n = 10
	type result struct {
		cs      *repo.Changeset
		existed bool
		err     error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			cs, existed, err := r.CreateChangeset(context.Background(), req, repo.CreateOptions{})
			results[i] = result{cs, existed, err}
		})
	}
	wg.Wait()
	created := 0
	for _, res := range results {
		if res.err != nil {
			t.Fatalf("concurrent create: %v", res.err)
		}
		if !res.existed {
			created++
		}
		if res.cs.ID != results[0].cs.ID || res.cs.Seq != results[0].cs.Seq || res.cs.Core != results[0].cs.Core {
			t.Errorf("results differ: %s/%d vs %s/%d", res.cs.ID.Short(8), res.cs.Seq, results[0].cs.ID.Short(8), results[0].cs.Seq)
		}
	}
	if created != 1 {
		t.Errorf("%d creates reported a new changeset, want exactly 1 (and %d already_existed)", created, n-1)
	}
	again, existed, err := r.CreateChangeset(context.Background(), req, repo.CreateOptions{})
	if err != nil || !existed || again.ID != results[0].cs.ID {
		t.Errorf("repeat create = %v, existed %v, %v", again, existed, err)
	}
}

func TestCreateErrors(t *testing.T) {
	t.Parallel()
	r := repotest.New(t)
	ctx := context.Background()
	root := repotest.Commit(t, r, nil, "root", repotest.Put(t, r, "f", "data"), repotest.Put(t, r, "d/x", "x"))
	parent := []hash.ID{root.ID}

	missingBlob := changeset.Change{Path: []byte("m"), Op: changeset.OpPut, Mode: object.ModeFile, Size: 1, Blob: hash.ID{1}}
	wrongSize := repotest.Put(t, r, "w", "four")
	wrongSize.Size = 5
	treeID, treeStored := object.Encode(object.KindTree, []byte{0})
	if err := r.Objects().Put(ctx, treeID, treeStored); err != nil {
		t.Fatal(err)
	}
	notABlob := changeset.Change{Path: []byte("t"), Op: changeset.OpPut, Mode: object.ModeFile, Size: 1, Blob: treeID}
	tooBig := repotest.Put(t, r, "big", "x")
	tooBig.Size = repo.DefaultMaxBlobSize + 1

	tests := []struct {
		name    string
		parents []hash.ID
		extras  map[string][]byte
		changes []changeset.Change
		want    error
	}{
		{"no changes", parent, nil, nil, repo.ErrInvalid},
		{"bad path", parent, nil, []changeset.Change{repotest.Put(t, r, ".xylem/x", "x")}, repo.ErrInvalid},
		{"unknown parent", []hash.ID{{9}}, nil, []changeset.Change{repotest.Put(t, r, "f", "y")}, repo.ErrNotFound},
		{"wrong size", parent, nil, []changeset.Change{wrongSize}, repo.ErrInvalid},
		{"tree as blob", parent, nil, []changeset.Change{notABlob}, repo.ErrInvalid},
		{"too big", parent, nil, []changeset.Change{tooBig}, repo.ErrInvalid},
		{"delete missing", parent, nil, []changeset.Change{repotest.Delete("nope")}, repo.ErrInvalid},
		{"no-op put", parent, nil, []changeset.Change{repotest.Put(t, r, "f", "data")}, repo.ErrInvalid},
		{"file/dir clash", parent, nil, []changeset.Change{repotest.Put(t, r, "f/inside", "x")}, repo.ErrInvalid},
		{"root without AllowRoot", nil, nil, []changeset.Change{repotest.Put(t, r, "f", "z")}, repo.ErrInvalid},
		{"bad git-sha", parent, map[string][]byte{changeset.ExtraGitSHA: []byte("XYZ")}, []changeset.Change{repotest.Put(t, r, "f", "q")}, repo.ErrInvalid},
		{"uppercase git-sha", parent, map[string][]byte{changeset.ExtraGitSHA: []byte(strings.Repeat("AB", 20))}, []changeset.Change{repotest.Put(t, r, "f", "q")}, repo.ErrInvalid},
	}
	for _, tt := range tests {
		_, _, err := r.CreateChangeset(ctx, repo.CreateRequest{Parents: tt.parents, Author: repotest.Author, Extras: tt.extras, Changes: tt.changes}, repo.CreateOptions{})
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}

	_, _, err := r.CreateChangeset(ctx, repo.CreateRequest{
		Parents: parent, Author: repotest.Author,
		Changes: []changeset.Change{missingBlob, repotest.Put(t, r, "ok", "fine")},
	}, repo.CreateOptions{})
	var missing *repo.MissingBlobsError
	if !errors.As(err, &missing) || missing.Total != 1 || missing.IDs[0] != missingBlob.Blob {
		t.Errorf("missing blob: err = %v", err)
	}
}

func TestCreateOptions(t *testing.T) {
	t.Parallel()
	r, m := repotest.NewWithMeta(t)
	ctx := context.Background()
	// A root in an empty repository needs no AllowRoot.
	root, _, err := r.CreateChangeset(ctx, repo.CreateRequest{Author: repotest.Author, Changes: []changeset.Change{repotest.Put(t, r, "f", "1")}}, repo.CreateOptions{})
	if err != nil {
		t.Fatalf("first root: %v", err)
	}
	// AllowEmpty: an imported empty commit.
	empty, _, err := r.CreateChangeset(ctx, repo.CreateRequest{Parents: []hash.ID{root.ID}, Author: repotest.Author, Message: []byte("empty")}, repo.CreateOptions{AllowEmpty: true})
	if err != nil || empty.Core.RootTree != root.Core.RootTree {
		t.Errorf("AllowEmpty: %v", err)
	}
	// TrustSizes skips the header read (the importer computed sizes itself).
	wrong := repotest.Put(t, r, "g", "four")
	wrong.Size = 99
	if _, _, err := r.CreateChangeset(ctx, repo.CreateRequest{Parents: []hash.ID{root.ID}, Author: repotest.Author, Changes: []changeset.Change{wrong}}, repo.CreateOptions{TrustSizes: true}); err != nil {
		t.Errorf("TrustSizes: %v", err)
	}
	// Explicit committer and git-sha extra.
	committer := changeset.Signature{NameEmail: []byte("Importer <i@x>"), Time: 42, TZMinutes: -180}
	sha := strings.Repeat("0a", 20)
	imported, _, err := r.CreateChangeset(ctx, repo.CreateRequest{
		Parents: []hash.ID{root.ID}, Author: repotest.Author,
		Extras: map[string][]byte{changeset.ExtraGitSHA: []byte(sha)}, Changes: []changeset.Change{repotest.Put(t, r, "h", "h")},
	},
		repo.CreateOptions{Committer: &committer})
	if err != nil || imported.Committer.TZMinutes != -180 || imported.Committer.Time != 42 {
		t.Fatalf("explicit committer: %+v, %v", imported, err)
	}
	shaBytes := make([]byte, 20)
	for i := range shaBytes {
		shaBytes[i] = 0x0a
	}
	if id, err := m.GetByGitSHA(ctx, 1, shaBytes); err != nil || id != imported.ID {
		t.Errorf("GetByGitSHA = %s, %v", id.Short(8), err)
	}
	// Mode-only change of an existing blob: no size re-check needed, and allowed.
	modeOnly := repotest.Put(t, r, "f", "1")
	modeOnly.Mode = object.ModeExec
	if _, _, err := r.CreateChangeset(ctx, repo.CreateRequest{Parents: []hash.ID{root.ID}, Author: repotest.Author, Changes: []changeset.Change{modeOnly}}, repo.CreateOptions{}); err != nil {
		t.Errorf("mode-only change: %v", err)
	}
}

// Acceptance: CreateChangeset of a 10-file change on a 1,000-file tree. The
// number is logged; the bound here is a loose sanity check (the reported p50
// comes from a run without -race).
func TestCreateChangesetLatency(t *testing.T) {
	r := repotest.New(t)
	var files []changeset.Change
	for i := range 1000 {
		files = append(files, repotest.Put(t, r, fmt.Sprintf("dir%02d/sub%d/file%03d.txt", i%20, i%5, i), fmt.Sprint("content ", i)))
	}
	tip := repotest.Commit(t, r, nil, "1000 files", files...)
	var durations []time.Duration
	for round := range 60 {
		var changes []changeset.Change
		for k := range 10 {
			i := (round*37 + k*101) % 1000
			changes = append(changes, repotest.Put(t, r, fmt.Sprintf("dir%02d/sub%d/file%03d.txt", i%20, i%5, i), fmt.Sprint("round ", round, " ", k)))
		}
		start := time.Now()
		cs, _, err := r.CreateChangeset(context.Background(), repo.CreateRequest{
			Parents: []hash.ID{tip.ID}, Author: repotest.Author,
			Message: []byte(fmt.Sprint("round ", round)), Changes: changes,
		}, repo.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if round >= 10 { // warm caches first
			durations = append(durations, time.Since(start))
		}
		tip = cs
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50, p99 := durations[len(durations)/2], durations[len(durations)*99/100]
	t.Logf("CreateChangeset, 10 files on a 1,000-file tree: p50 %v, p99 %v (n=%d)", p50, p99, len(durations))
	if p50 > 100*time.Millisecond {
		t.Errorf("p50 %v is far above the 30 ms budget", p50)
	}
	_ = slices.Max(durations)
}
