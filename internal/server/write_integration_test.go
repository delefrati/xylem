//go:build integration

package server_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/store"
)

var author = &xylemv1.Signature{NameEmail: []byte("Ada <ada@example.com>"), Time: timestamppb.New(timestamppb.Now().AsTime().Truncate(1e9)), TzOffsetMinutes: 60}

// uploadBlob puts content with PutObjects and returns a PUT change for path.
func uploadBlob(t *testing.T, c *clients, path, content string) *xylemv1.FileChange {
	t.Helper()
	o := storedObject(object.KindBlob, []byte(content))
	if _, err := putObjects(t, c, &xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{o}}); err != nil {
		t.Fatal(err)
	}
	return &xylemv1.FileChange{Path: []byte(path), Op: xylemv1.FileChange_OP_PUT, Mode: xylemv1.FileMode_FILE_MODE_FILE, Size: uint64(len(content)), BlobId: o.Id}
}

func TestCreateChangesetRPC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := startServer(t)

	readme := uploadBlob(t, c, "README", "hello\n")
	req := &xylemv1.CreateChangesetRequest{Author: author, Message: []byte("root\n"), Changes: []*xylemv1.FileChange{readme}}
	resp, err := c.Write.CreateChangeset(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	root := resp.GetChangeset()
	if resp.GetAlreadyExisted() || len(root.GetId()) != 32 || len(root.GetRootTree()) != 32 || root.GetChangeCount() != 1 {
		t.Fatalf("root = %v", resp)
	}
	if string(root.GetCommitter().GetNameEmail()) != "Ada <ada@example.com>" || root.GetCommitter().GetTzOffsetMinutes() != 0 {
		t.Errorf("committer %v, want the author at the server clock (tz 0)", root.GetCommitter())
	}
	again, err := c.Write.CreateChangeset(ctx, req)
	if err != nil || !again.GetAlreadyExisted() || string(again.GetChangeset().GetId()) != string(root.GetId()) {
		t.Errorf("idempotent create = %v, %v", again, err)
	}

	// A child: modify and add; the files are readable at the new id.
	child, err := c.Write.CreateChangeset(ctx, &xylemv1.CreateChangesetRequest{
		Parents: [][]byte{root.GetId()}, Author: author, Message: []byte("child\n"),
		Changes: []*xylemv1.FileChange{uploadBlob(t, c, "src/a.go", "package a\n"), uploadBlob(t, c, "README", "hello again\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.GetChangeset().GetGeneration() != 2 || child.GetChangeset().GetOnMainline() {
		t.Errorf("child = %v", child.GetChangeset())
	}
	_, content, err := readFile(t, c, fmt.Sprintf("%x", child.GetChangeset().GetId()), "src/a.go")
	if err != nil || string(content) != "package a\n" {
		t.Errorf("read src/a.go at child = %q, %v", content, err)
	}

	missingBlobs := make([]*xylemv1.FileChange, 25)
	for i := range missingBlobs {
		id, _ := object.Encode(object.KindBlob, []byte(fmt.Sprint(i)))
		missingBlobs[i] = &xylemv1.FileChange{Path: []byte(fmt.Sprintf("m%02d", i)), Op: xylemv1.FileChange_OP_PUT, Mode: xylemv1.FileMode_FILE_MODE_FILE, Size: 1, BlobId: id[:]}
	}
	parents := [][]byte{root.GetId()}
	put := func(path string, size uint64) *xylemv1.FileChange {
		fc := uploadBlob(t, c, "x", "x\n")
		fc.Path, fc.Size = []byte(path), size
		return fc
	}
	del := func(path string) *xylemv1.FileChange {
		return &xylemv1.FileChange{Path: []byte(path), Op: xylemv1.FileChange_OP_DELETE}
	}
	for name, tc := range map[string]struct {
		req  *xylemv1.CreateChangesetRequest
		code codes.Code
		msg  string
	}{
		"missing blobs":      {&xylemv1.CreateChangesetRequest{Parents: parents, Author: author, Changes: missingBlobs}, codes.FailedPrecondition, "25 missing blobs"},
		"unknown parent":     {&xylemv1.CreateChangesetRequest{Parents: [][]byte{make([]byte, 32)}, Author: author, Changes: []*xylemv1.FileChange{put("y", 2)}}, codes.NotFound, "parent"},
		"short parent id":    {&xylemv1.CreateChangesetRequest{Parents: [][]byte{{1}}, Author: author, Changes: []*xylemv1.FileChange{put("y", 2)}}, codes.InvalidArgument, "parents[0]"},
		"bad path":           {&xylemv1.CreateChangesetRequest{Parents: parents, Author: author, Changes: []*xylemv1.FileChange{put("a//b", 2)}}, codes.InvalidArgument, ""},
		"size mismatch":      {&xylemv1.CreateChangesetRequest{Parents: parents, Author: author, Changes: []*xylemv1.FileChange{put("y", 3)}}, codes.InvalidArgument, "declared"},
		"no-op put":          {&xylemv1.CreateChangesetRequest{Parents: parents, Author: author, Changes: []*xylemv1.FileChange{readme}}, codes.InvalidArgument, ""},
		"delete missing":     {&xylemv1.CreateChangesetRequest{Parents: parents, Author: author, Changes: []*xylemv1.FileChange{del("nope")}}, codes.InvalidArgument, ""},
		"file/dir clash":     {&xylemv1.CreateChangesetRequest{Parents: parents, Author: author, Changes: []*xylemv1.FileChange{put("README/x", 2)}}, codes.InvalidArgument, ""},
		"no changes":         {&xylemv1.CreateChangesetRequest{Parents: parents, Author: author}, codes.InvalidArgument, ""},
		"second root":        {&xylemv1.CreateChangesetRequest{Author: author, Changes: []*xylemv1.FileChange{put("y", 2)}}, codes.InvalidArgument, "root"},
		"no author time":     {&xylemv1.CreateChangesetRequest{Parents: parents, Author: &xylemv1.Signature{NameEmail: []byte("a <a@b>")}, Changes: []*xylemv1.FileChange{put("y", 2)}}, codes.InvalidArgument, "time"},
		"nanos in time":      {&xylemv1.CreateChangesetRequest{Parents: parents, Author: &xylemv1.Signature{NameEmail: []byte("a <a@b>"), Time: &timestamppb.Timestamp{Seconds: 1, Nanos: 5}}, Changes: []*xylemv1.FileChange{put("y", 2)}}, codes.InvalidArgument, "nanos"},
		"delete with blob":   {&xylemv1.CreateChangesetRequest{Parents: parents, Author: author, Changes: []*xylemv1.FileChange{{Path: []byte("README"), Op: xylemv1.FileChange_OP_DELETE, BlobId: readme.BlobId}}}, codes.InvalidArgument, "DELETE"},
		"unspecified op":     {&xylemv1.CreateChangesetRequest{Parents: parents, Author: author, Changes: []*xylemv1.FileChange{{Path: []byte("y")}}}, codes.InvalidArgument, "op"},
		"tree mode put":      {&xylemv1.CreateChangesetRequest{Parents: parents, Author: author, Changes: []*xylemv1.FileChange{{Path: []byte("y"), Op: xylemv1.FileChange_OP_PUT, Mode: xylemv1.FileMode_FILE_MODE_TREE, Size: 2, BlobId: readme.BlobId}}}, codes.InvalidArgument, ""},
		"unknown repository": {&xylemv1.CreateChangesetRequest{Repo: "nope", Parents: parents, Author: author, Changes: []*xylemv1.FileChange{put("y", 2)}}, codes.NotFound, ""},
	} {
		_, err := c.Write.CreateChangeset(ctx, tc.req)
		if status.Code(err) != tc.code || !strings.Contains(status.Convert(err).Message(), tc.msg) {
			t.Errorf("%s: %v, want %v containing %q", name, err, tc.code, tc.msg)
		}
	}
	// Missing blobs: at most 20 ids are listed.
	_, err = c.Write.CreateChangeset(ctx, &xylemv1.CreateChangesetRequest{Parents: parents, Author: author, Changes: missingBlobs})
	if n := strings.Count(status.Convert(err).Message(), ", ") + 1; n != 20 {
		t.Errorf("missing blobs message lists %d ids: %v", n, err)
	}
}

// metricValue returns a counter's value or a histogram's sample count.
func metricValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		m := f.GetMetric()[0]
		if h := m.GetHistogram(); h != nil {
			return float64(h.GetSampleCount())
		}
		return m.GetCounter().GetValue()
	}
	return 0
}

func TestRebaseAndLandRPCs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	c := startServerFor(t, repotest.NewEnv(t, store.NewMem()), func(cfg *server.Config) { cfg.Registry = reg })
	create := func(parent []byte, msg string, changes ...*xylemv1.FileChange) []byte {
		t.Helper()
		req := &xylemv1.CreateChangesetRequest{Author: author, Message: []byte(msg), Changes: changes}
		if parent != nil {
			req.Parents = [][]byte{parent}
		}
		resp, err := c.Write.CreateChangeset(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetChangeset().GetId()
	}
	root := create(nil, "root", uploadBlob(t, c, "README", "r\n"), uploadBlob(t, c, "a.txt", "a\n"))
	if _, err := c.Repo.UpdateRef(ctx, &xylemv1.UpdateRefRequest{Name: "main", NewId: root}); err != nil {
		t.Fatal(err)
	}
	m1 := create(root, "m1", uploadBlob(t, c, "README", "r2\n"))
	if _, err := c.Repo.UpdateRef(ctx, &xylemv1.UpdateRefRequest{Name: "main", ExpectedId: root, NewId: m1}); err != nil {
		t.Fatal(err)
	}
	x1 := create(root, "x1", uploadBlob(t, c, "b.txt", "b\n"))
	x2 := create(x1, "x2", uploadBlob(t, c, "c.txt", "c\n"))

	rb, err := c.Write.Rebase(ctx, &xylemv1.RebaseRequest{Stack: [][]byte{x1, x2}, Onto: "main"})
	if err != nil || len(rb.GetRebased()) != 2 || len(rb.GetConflicts()) != 0 {
		t.Fatalf("rebase = %v, %v", rb, err)
	}
	cs, err := c.Repo.GetChangesets(ctx, &xylemv1.GetChangesetsRequest{Ids: rb.GetRebased()[:1]})
	if err != nil || string(cs.GetChangesets()[0].GetParents()[0]) != string(m1) || string(cs.GetChangesets()[0].GetExtras()["rebased-from"]) != fmt.Sprintf("%x", x1) {
		t.Errorf("rebased changeset %v, %v", cs, err)
	}
	if rb, err := c.Write.Rebase(ctx, &xylemv1.RebaseRequest{Stack: [][]byte{x1, x2}, Onto: fmt.Sprintf("%x", root)}); err != nil ||
		string(rb.GetRebased()[0]) != string(x1) {
		t.Errorf("rebase onto its own base = %v, %v", rb, err)
	}

	land, err := c.Write.Land(ctx, &xylemv1.LandRequest{Stack: [][]byte{x1, x2}})
	if err != nil || len(land.GetLanded()) != 2 || land.GetAttempts() != 1 || string(land.GetNewTip()) != string(land.GetLanded()[1]) {
		t.Fatalf("land = %v, %v", land, err)
	}
	tip, err := c.Repo.ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Revision: "main"})
	if err != nil || string(tip.GetId()) != string(land.GetNewTip()) {
		t.Errorf("main = %v after land, %v", tip, err)
	}
	on, err := c.Repo.GetChangesets(ctx, &xylemv1.GetChangesetsRequest{Ids: land.GetLanded()})
	if err != nil || !on.GetChangesets()[0].GetOnMainline() || !on.GetChangesets()[1].GetOnMainline() {
		t.Errorf("landed changesets not on the mainline: %v", err)
	}

	// A conflicting land: the response carries the conflict; main stays.
	y := create(root, "y", uploadBlob(t, c, "README", "r by y\n"))
	land, err = c.Write.Land(ctx, &xylemv1.LandRequest{Stack: [][]byte{y}})
	if err != nil || len(land.GetConflicts()) != 1 || string(land.GetConflictingChangeset()) != string(y) || len(land.GetNewTip()) != 0 {
		t.Fatalf("conflicting land = %v, %v", land, err)
	}
	got := land.GetConflicts()[0]
	if string(got.GetPath()) != "README" || got.GetKind() != xylemv1.Conflict_KIND_CONTENT || len(got.GetBaseBlob()) != 32 || len(got.GetOursBlob()) != 32 || len(got.GetTheirsBlob()) != 32 {
		t.Errorf("conflict %v", got)
	}
	if after, _ := c.Repo.ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Revision: "main"}); string(after.GetId()) != string(tip.GetId()) {
		t.Error("a conflicting land moved main")
	}
	if rb, err := c.Write.Rebase(ctx, &xylemv1.RebaseRequest{Stack: [][]byte{y}, Onto: "main"}); err != nil || len(rb.GetConflicts()) != 1 || len(rb.GetRebased()) != 0 {
		t.Errorf("conflicting rebase = %v, %v", rb, err)
	}
	if n := metricValue(t, reg, "xylem_land_attempts"); n != 2 {
		t.Errorf("xylem_land_attempts count %v, want 2", n)
	}
	if n := metricValue(t, reg, "xylem_land_conflicts_total"); n != 1 {
		t.Errorf("xylem_land_conflicts_total %v, want 1", n)
	}
	if n := metricValue(t, reg, "xylem_merge_conflict_total"); n != 2 { // the land and the rebase of y
		t.Errorf("xylem_merge_conflict_total %v, want 2", n)
	}

	// Different regions of one file merge line by line (M7).
	tip, err = c.Repo.ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Revision: "main"})
	if err != nil {
		t.Fatal(err)
	}
	base := create(tip.GetId(), "add lines", uploadBlob(t, c, "lines.txt", "1\n2\n3\n4\n5\n"))
	if _, err := c.Write.Land(ctx, &xylemv1.LandRequest{Stack: [][]byte{base}}); err != nil {
		t.Fatal(err)
	}
	top := create(base, "top", uploadBlob(t, c, "lines.txt", "one\n2\n3\n4\n5\n"))
	bottom := create(base, "bottom", uploadBlob(t, c, "lines.txt", "1\n2\n3\n4\nfive\n"))
	for _, id := range [][]byte{top, bottom} {
		if land, err := c.Write.Land(ctx, &xylemv1.LandRequest{Stack: [][]byte{id}}); err != nil || len(land.GetConflicts()) != 0 {
			t.Fatalf("land of a different region = %v, %v", land, err)
		}
	}
	if _, merged, err := readFile(t, c, "main", "lines.txt"); err != nil || string(merged) != "one\n2\n3\n4\nfive\n" {
		t.Errorf("merged lines.txt = %q, %v", merged, err)
	}
	if n := metricValue(t, reg, "xylem_merge_clean_total"); n != 1 {
		t.Errorf("xylem_merge_clean_total %v, want 1", n)
	}

	for name, tc := range map[string]struct {
		call func() error
		code codes.Code
	}{
		"land: bad id":      {func() error { _, err := c.Write.Land(ctx, &xylemv1.LandRequest{Stack: [][]byte{{1}}}); return err }, codes.InvalidArgument},
		"land: empty stack": {func() error { _, err := c.Write.Land(ctx, &xylemv1.LandRequest{}); return err }, codes.InvalidArgument},
		"land: not a chain": {func() error { _, err := c.Write.Land(ctx, &xylemv1.LandRequest{Stack: [][]byte{x2, x1}}); return err }, codes.InvalidArgument},
		"land: too many": {func() error {
			_, err := c.Write.Land(ctx, &xylemv1.LandRequest{Stack: [][]byte{x1}, MaxAttempts: 5000})
			return err
		}, codes.InvalidArgument},
		"land: unknown ref": {func() error {
			_, err := c.Write.Land(ctx, &xylemv1.LandRequest{Stack: [][]byte{x1}, TargetRef: "nope"})
			return err
		}, codes.NotFound},
		"land: unknown id": {func() error {
			_, err := c.Write.Land(ctx, &xylemv1.LandRequest{Stack: [][]byte{make([]byte, 32)}})
			return err
		}, codes.NotFound},
		"rebase: bad onto": {func() error {
			_, err := c.Write.Rebase(ctx, &xylemv1.RebaseRequest{Stack: [][]byte{x1}, Onto: "nope"})
			return err
		}, codes.NotFound},
		"rebase: empty onto": {func() error { _, err := c.Write.Rebase(ctx, &xylemv1.RebaseRequest{Stack: [][]byte{x1}}); return err }, codes.InvalidArgument},
		"rebase: unknown repo": {func() error {
			_, err := c.Write.Rebase(ctx, &xylemv1.RebaseRequest{Repo: "nope", Stack: [][]byte{x1}, Onto: "main"})
			return err
		}, codes.NotFound},
	} {
		if err := tc.call(); status.Code(err) != tc.code {
			t.Errorf("%s: %v, want %v", name, err, tc.code)
		}
	}
}
