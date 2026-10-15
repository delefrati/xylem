//go:build integration

package server_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/object"
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
