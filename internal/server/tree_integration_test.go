//go:build integration

package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/importer"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/testutil"
	"github.com/delefrati/xylem/internal/treeops"
)

// importFixture imports a git fixture into the server's repo.
func importFixture(t *testing.T, c *clients, profile string) *gitsource.Repo {
	t.Helper()
	src, err := gitsource.Open(context.Background(), testutil.GitFixture(t, profile), gitsource.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	im, err := importer.New(c.Env.Repo, src, importer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := im.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	return src
}

func listAll(t *testing.T, c *clients, req *xylemv1.ListDirectoryRequest, between func()) []*xylemv1.DirEntry {
	t.Helper()
	var all []*xylemv1.DirEntry
	for {
		resp, err := c.Repo.ListDirectory(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, resp.Entries...)
		if resp.NextPageToken == "" {
			return all
		}
		if between != nil {
			between()
			between = nil
		}
		req.Page = &xylemv1.PageRequest{PageSize: req.GetPage().GetPageSize(), PageToken: resp.NextPageToken}
	}
}

func readFile(t *testing.T, c *clients, rev, path string) ([]*xylemv1.ReadFileResponse, []byte, error) {
	t.Helper()
	stream, err := c.Repo.ReadFile(context.Background(), &xylemv1.ReadFileRequest{Revision: rev, Path: []byte(path)})
	if err != nil {
		return nil, nil, err
	}
	var msgs []*xylemv1.ReadFileResponse
	var content []byte
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return msgs, content, nil
		}
		if err != nil {
			return msgs, content, err
		}
		msgs = append(msgs, m)
		content = append(content, m.Chunk...)
	}
}

func TestTreeRPCs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := startServer(t)
	src := importFixture(t, c, "edge")
	tip, _ := src.ResolveBranch(ctx, "main")
	gitEntries, err := src.LsTree(ctx, tip)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint64{}
	for _, e := range gitEntries {
		if e.Mode != gitsource.ModeSubmodule && e.Size >= 0 {
			want[string(e.Path)] = uint64(e.Size)
		}
	}

	// Recursive listing, paged by 7, with main moving between pages.
	r := c.Env.Repo
	moveMain := func() {
		main, _ := r.GetRef(ctx, repo.MainRef)
		next := repotest.Commit(t, r, []hash.ID{main.ID}, "moves main", repotest.Put(t, r, "NEW-FILE", "new"))
		if _, err := r.UpdateRef(ctx, repo.MainRef, main.ID, next.ID, false, "t", "admin"); err != nil {
			t.Fatal(err)
		}
	}
	all := listAll(t, c, &xylemv1.ListDirectoryRequest{Recursive: true, Page: &xylemv1.PageRequest{PageSize: 7}}, moveMain)
	got := map[string]uint64{}
	var order [][]byte
	for _, e := range all {
		got[string(e.Name)] = e.Size
		order = append(order, e.Name)
		if e.Mode == xylemv1.FileMode_FILE_MODE_TREE {
			t.Errorf("recursive listing returned directory %q", e.Name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("recursive listing has %d files, git %d (the page token must pin the tree)", len(got), len(want))
	}
	for p, size := range want {
		if got[p] != size {
			t.Errorf("%q: size %d, git %d", p, got[p], size)
		}
	}
	if !slices.IsSortedFunc(order, treeops.ComparePathsDFS) {
		t.Error("recursive listing not in depth-first path order")
	}

	// After the move, a fresh listing sees NEW-FILE.
	root := listAll(t, c, &xylemv1.ListDirectoryRequest{}, nil)
	names := map[string]xylemv1.FileMode{}
	for _, e := range root {
		names[string(e.Name)] = e.Mode
	}
	if names["NEW-FILE"] != xylemv1.FileMode_FILE_MODE_FILE || names["dup"] != xylemv1.FileMode_FILE_MODE_TREE || names["run.sh"] != xylemv1.FileMode_FILE_MODE_EXEC {
		t.Errorf("root listing = %v", names)
	}

	// A subdirectory lists full paths; paging non-recursively by 2.
	sub := listAll(t, c, &xylemv1.ListDirectoryRequest{Revision: "main~1", Path: []byte("dup"), Recursive: true, Page: &xylemv1.PageRequest{PageSize: 2}}, nil)
	var subNames []string
	for _, e := range sub {
		subNames = append(subNames, string(e.Name))
	}
	if !slices.Equal(subNames, []string{"dup/b.txt", "dup/c/d.txt"}) {
		t.Errorf("dup listing = %v", subNames)
	}
	flat := listAll(t, c, &xylemv1.ListDirectoryRequest{Page: &xylemv1.PageRequest{PageSize: 2}}, nil)
	if len(flat) != len(root) {
		t.Errorf("non-recursive paging: %d entries, want %d", len(flat), len(root))
	}

	for name, req := range map[string]*xylemv1.ListDirectoryRequest{
		"file as dir": {Path: []byte("README")},
		"bad path":    {Path: []byte("/abs")},
		"bad token":   {Page: &xylemv1.PageRequest{PageToken: "!!!"}},
	} {
		if _, err := c.Repo.ListDirectory(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.Repo.ListDirectory(ctx, &xylemv1.ListDirectoryRequest{Path: []byte("nope")}); status.Code(err) != codes.NotFound {
		t.Errorf("missing dir: %v", err)
	}

	// ReadFile: byte-identical to git, chunked at ≤ 1 MiB.
	for _, path := range []string{"big.bin", "x", "empty.txt", "latin1-\xe9.txt", "new\nline.txt", "run.sh"} {
		e := findGit(t, gitEntries, path)
		wantContent, err := src.Blob(ctx, e.SHA)
		if err != nil {
			t.Fatal(err)
		}
		msgs, content, err := readFile(t, c, "main~1", path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		if !bytes.Equal(content, wantContent) {
			t.Errorf("ReadFile(%q): %d bytes differ from git", path, len(content))
		}
		if msgs[0].Size != uint64(len(wantContent)) || len(msgs[0].BlobId) != 32 || msgs[0].Mode == xylemv1.FileMode_FILE_MODE_UNSPECIFIED {
			t.Errorf("ReadFile(%q) first message = mode %v size %d", path, msgs[0].Mode, msgs[0].Size)
		}
		for i, m := range msgs {
			if len(m.Chunk) > 1<<20 || (i > 0 && (m.Size != 0 || len(m.BlobId) != 0)) {
				t.Errorf("ReadFile(%q) message %d malformed", path, i)
			}
		}
		if path == "big.bin" && len(msgs) != 11 {
			t.Errorf("big.bin came in %d messages, want 11", len(msgs))
		}
	}
	if msgs, _, _ := readFile(t, c, "main~1", "x"); msgs[0].Mode != xylemv1.FileMode_FILE_MODE_FILE {
		t.Errorf("x mode %v", msgs[0].Mode)
	}
	for path, code := range map[string]codes.Code{"dup": codes.InvalidArgument, "nope": codes.NotFound, "": codes.InvalidArgument, "a//b": codes.InvalidArgument} {
		if _, _, err := readFile(t, c, "", path); status.Code(err) != code {
			t.Errorf("ReadFile(%q) = %v, want %v", path, err, code)
		}
	}
	if _, _, err := readFile(t, c, "nope", "README"); status.Code(err) != codes.NotFound {
		t.Errorf("ReadFile at unknown revision: %v", err)
	}
}

func findGit(t *testing.T, entries []gitsource.TreeEntry, path string) gitsource.TreeEntry {
	t.Helper()
	for _, e := range entries {
		if string(e.Path) == path {
			return e
		}
	}
	t.Fatalf("no git entry %q", path)
	return gitsource.TreeEntry{}
}
