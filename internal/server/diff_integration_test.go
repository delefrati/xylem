//go:build integration

package server_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/diff"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/testutil"
	"github.com/delefrati/xylem/internal/treeops"
)

func diffAll(t *testing.T, c *clients, req *xylemv1.DiffRequest) ([]*xylemv1.FileDiff, error) {
	t.Helper()
	stream, err := c.Repo.Diff(context.Background(), req)
	if err != nil {
		return nil, err
	}
	var files []*xylemv1.FileDiff
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, err
		}
		files = append(files, m.Files...)
	}
}

// gitNameStatus returns path → status letter from git, submodules excluded.
func gitNameStatus(t *testing.T, dir string, args ...string) map[string]string {
	t.Helper()
	out := testutil.Git(t, dir, append([]string{"diff", "--name-status", "--no-renames", "-z"}, args...)...)
	raw := testutil.Git(t, dir, append([]string{"diff", "--raw", "--no-renames", "-z"}, args...)...)
	sub := map[string]bool{}
	for _, rec := range strings.Split(raw, "\x00:") {
		if strings.Contains(rec, "160000") {
			if f := strings.Split(rec, "\x00"); len(f) > 1 {
				sub[f[1]] = true
			}
		}
	}
	m := map[string]string{}
	f := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		if !sub[f[i+1]] {
			m[f[i+1]] = f[i]
		}
	}
	return m
}

// compatible maps git's letters to ours: T (type change) and M are MODIFIED
// or MODE_CHANGED depending on whether the blob changed.
func compatible(git string, ours xylemv1.FileDiff_Status) bool {
	switch git {
	case "A":
		return ours == xylemv1.FileDiff_STATUS_ADDED
	case "D":
		return ours == xylemv1.FileDiff_STATUS_DELETED
	case "M", "T":
		return ours == xylemv1.FileDiff_STATUS_MODIFIED || ours == xylemv1.FileDiff_STATUS_MODE_CHANGED
	}
	return false
}

func checkDiff(t *testing.T, c *clients, dir, label string, req *xylemv1.DiffRequest, gitArgs ...string) {
	t.Helper()
	files, err := diffAll(t, c, req)
	if err != nil {
		t.Fatalf("%s: Diff: %v", label, err)
	}
	want := gitNameStatus(t, dir, gitArgs...)
	got := map[string]*xylemv1.FileDiff{}
	var order [][]byte
	for _, f := range files {
		got[string(f.Path)] = f
		order = append(order, f.Path)
	}
	if len(got) != len(want) {
		t.Errorf("%s: %d files, git %d (%v vs %v)", label, len(got), len(want), keys(got), want)
	}
	for p, st := range want {
		if f, ok := got[p]; !ok || !compatible(st, f.Status) {
			t.Errorf("%s: %q git %s, xylem %v", label, p, st, f.GetStatus())
		}
	}
	if !slices.IsSortedFunc(order, treeops.ComparePathsDFS) {
		t.Errorf("%s: files not in depth-first path order", label)
	}
	if !req.IncludeHunks {
		return
	}
	// Applying the hunks to the old file yields the new file.
	for _, f := range files {
		if f.Binary || f.Status == xylemv1.FileDiff_STATUS_MODE_CHANGED {
			continue
		}
		var old, newC []byte
		if len(f.OldBlob) > 0 {
			old = blobContent(t, c, f.OldBlob)
		}
		if len(f.NewBlob) > 0 {
			newC = blobContent(t, c, f.NewBlob)
		}
		var hunks []diff.Hunk
		for _, h := range f.Hunks {
			hunks = append(hunks, diff.Hunk{OldStart: int(h.OldStart), OldLines: int(h.OldLines), NewStart: int(h.NewStart), NewLines: int(h.NewLines), Body: h.Body})
		}
		if got, err := diff.Apply(old, hunks); err != nil || string(got) != string(newC) {
			t.Errorf("%s: hunks for %q do not reproduce the new file: %v", label, f.Path, err)
		}
	}
}

func keys(m map[string]*xylemv1.FileDiff) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func blobContent(t *testing.T, c *clients, id []byte) []byte {
	t.Helper()
	stream, err := c.Object.GetObjects(context.Background(), &xylemv1.GetObjectsRequest{Ids: [][]byte{id}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := stream.Recv()
	if err != nil || len(m.Objects) != 1 {
		t.Fatalf("blob %x: %v", id, err)
	}
	body, err := decodeBlob(m.Objects[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestDiffRPC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := startServer(t)
	src := importFixture(t, c, "edge")
	dir := testutil.GitFixture(t, "edge")
	shaMap, err := c.Env.Repo.GitSHAMap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tip, _ := src.ResolveBranch(ctx, "main")
	var shas []gitsource.SHA
	for sha, err := range src.RevList(ctx, tip) {
		if err != nil {
			t.Fatal(err)
		}
		shas = append(shas, sha)
	}
	// Every commit against its first parent (fast path), with hunks.
	for _, sha := range shas {
		id := shaMap[sha]
		commit, _ := src.ReadCommit(ctx, sha)
		gitArgs := []string{sha.String() + "^", sha.String()}
		if len(commit.Parents) == 0 {
			gitArgs = []string{"4b825dc642cb6eb9a060e54bf8d69288fbee4904", sha.String()} // git's empty tree
		}
		checkDiff(t, c, dir, "parent diff "+sha.String()[:8], &xylemv1.DiffRequest{ToRevision: id.String(), IncludeHunks: true}, gitArgs...)
	}
	// Arbitrary pairs (tree walk), both directions.
	for _, pair := range [][2]int{{0, len(shas) - 1}, {3, 12}, {len(shas) - 1, 5}, {7, 8}} {
		a, b := shas[pair[0]], shas[pair[1]]
		checkDiff(t, c, dir, "pair", &xylemv1.DiffRequest{FromRevision: shaMap[a].String(), ToRevision: shaMap[b].String(), IncludeHunks: true}, a.String(), b.String())
	}
	// Path filter.
	first, last := shas[0], shas[len(shas)-1]
	checkDiff(t, c, dir, "filtered", &xylemv1.DiffRequest{FromRevision: shaMap[first].String(), ToRevision: shaMap[last].String(), Paths: [][]byte{[]byte("dup"), []byte("x")}},
		first.String(), last.String(), "--", "dup", "x")

	// Binary files are flagged; context_lines changes hunk sizes.
	files, err := diffAll(t, c, &xylemv1.DiffRequest{ToRevision: "git:" + findSubject(t, dir, "large binary"), IncludeHunks: true})
	if err != nil || len(files) != 1 || !files[0].Binary || len(files[0].Hunks) != 0 {
		t.Errorf("binary diff = %v, %v", files, err)
	}
	if _, err := diffAll(t, c, &xylemv1.DiffRequest{Paths: [][]byte{[]byte("/bad")}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad path filter: %v", err)
	}

	// The scan budget turns a huge diff into ResourceExhausted.
	small := startServerFor(t, repotest.NewEnv(t, store.NewMem()), func(cfg *server.Config) { cfg.MaxDiffEntries = 3 })
	importFixture(t, small, "edge")
	if _, err := diffAll(t, small, &xylemv1.DiffRequest{FromRevision: shaMap[first].String(), ToRevision: "main"}); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("over budget: %v", err)
	}
}

func findSubject(t *testing.T, dir, subject string) string {
	t.Helper()
	return testutil.Git(t, dir, "log", "--format=%H", "--fixed-strings", "--grep", subject, "-n1", "main")
}
