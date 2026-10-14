package importer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/testutil"
)

func gsha(b byte) gitsource.SHA {
	var s gitsource.SHA
	s[0], s[19] = b, b
	return s
}

// fakeBlobs maps any git sha to a deterministic Xylem id and a size.
func fakeBlobs(s gitsource.SHA) (BlobInfo, bool) {
	return BlobInfo{ID: hash.Sum(nil, s[:]), Size: uint64(s[0])}, true
}

func commit(parents ...gitsource.SHA) *gitsource.Commit {
	return &gitsource.Commit{
		SHA: gsha(0xcc), Parents: parents,
		Author:    gitsource.Ident{NameEmail: []byte("J\xf6rg <j@x>"), Time: 100, TZMinutes: 330},
		Committer: gitsource.Ident{NameEmail: []byte("C <c@x>"), Time: 200, TZMinutes: -180},
		Message:   []byte("msg\n"),
	}
}

func TestMapCommit(t *testing.T) {
	t.Parallel()
	c := commit(gsha(1))
	c.Encoding = "ISO-8859-1"
	changes := []gitsource.Change{
		{Status: 'A', NewMode: gitsource.ModeFile, NewSHA: gsha(2), Path: []byte("new.txt")},
		{Status: 'M', OldMode: gitsource.ModeFile, NewMode: gitsource.ModeExec, OldSHA: gsha(3), NewSHA: gsha(3), Path: []byte("run.sh")},
		{Status: 'T', OldMode: gitsource.ModeFile, NewMode: gitsource.ModeSymlink, OldSHA: gsha(4), NewSHA: gsha(5), Path: []byte("link")},
		{Status: 'D', OldMode: gitsource.ModeFile, OldSHA: gsha(6), Path: []byte("gone")},
		{Status: 'A', NewMode: gitsource.ModeSubmodule, NewSHA: gsha(7), Path: []byte("vendor/a")},
		{Status: 'D', OldMode: gitsource.ModeSubmodule, OldSHA: gsha(8), Path: []byte("vendor/b")},
		{Status: 'T', OldMode: gitsource.ModeFile, NewMode: gitsource.ModeSubmodule, OldSHA: gsha(9), NewSHA: gsha(10), Path: []byte("was-file")},
		{Status: 'T', OldMode: gitsource.ModeSubmodule, NewMode: gitsource.ModeFile, OldSHA: gsha(11), NewSHA: gsha(12), Path: []byte("now-file")},
		{Status: 'M', OldMode: 0o100664, NewMode: gitsource.ModeFile, OldSHA: gsha(13), NewSHA: gsha(13), Path: []byte("old-mode")},
		{Status: 'A', NewMode: gitsource.ModeFile, NewSHA: gsha(14), Path: []byte("caf\xe9")},
	}
	parent := hash.ID{1}
	m, err := MapCommit(c, changes, []hash.ID{parent}, fakeBlobs)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]changeset.Change{}
	for _, ch := range m.Request.Changes {
		got[string(ch.Path)] = ch
	}
	want := map[string]struct {
		op   changeset.Op
		mode object.Mode
	}{
		"new.txt":  {changeset.OpPut, object.ModeFile},
		"run.sh":   {changeset.OpPut, object.ModeExec},
		"link":     {changeset.OpPut, object.ModeSymlink},
		"gone":     {changeset.OpDelete, 0},
		"was-file": {changeset.OpDelete, 0},
		"now-file": {changeset.OpPut, object.ModeFile},
		"caf\xe9":  {changeset.OpPut, object.ModeFile},
	}
	if len(got) != len(want) {
		t.Errorf("mapped %d changes, want %d: %v", len(got), len(want), got)
	}
	for p, w := range want {
		if ch, ok := got[p]; !ok || ch.Op != w.op || ch.Mode != w.mode {
			t.Errorf("%q = %+v, want %v %v", p, ch, w.op, w.mode)
		}
	}
	if info, _ := fakeBlobs(gsha(2)); got["new.txt"].Blob != info.ID || got["new.txt"].Size != info.Size {
		t.Error("PUT blob id or size not from the blob lookup")
	}
	if string(m.Request.Extras[changeset.ExtraGitSHA]) != gsha(0xcc).String() ||
		string(m.Request.Extras[changeset.ExtraGitEncoding]) != "ISO-8859-1" ||
		string(m.Request.Extras[ExtraSubmodules]) != "vendor/a\nvendor/b\nwas-file\nnow-file" {
		t.Errorf("extras = %q", m.Request.Extras)
	}
	if string(m.Request.Author.NameEmail) != "J\xf6rg <j@x>" || m.Request.Author.TZMinutes != 330 ||
		m.Committer.Time != 200 || m.Committer.TZMinutes != -180 || m.Request.Parents[0] != parent {
		t.Errorf("signatures or parents wrong: %+v %+v", m.Request.Author, m.Committer)
	}
	if err := (&changeset.Changeset{
		Parents: m.Request.Parents, Author: m.Request.Author, Committer: m.Committer,
		Message: m.Request.Message, Extras: m.Request.Extras, Changes: m.Request.Changes,
	}).ValidateShape(changeset.ShapeOptions{}); err != nil {
		t.Errorf("mapped changeset fails shape validation: %v", err)
	}
	if blobs := BlobsNeeded(changes); len(blobs) != 6 {
		t.Errorf("BlobsNeeded = %d shas, want 6 (no submodules, no deletes, deduplicated)", len(blobs))
	}
}

func TestMapCommitErrors(t *testing.T) {
	t.Parallel()
	reserved := []gitsource.Change{{Status: 'A', NewMode: gitsource.ModeFile, NewSHA: gsha(1), Path: []byte("a/.xylem/x")}}
	if _, err := MapCommit(commit(), reserved, nil, fakeBlobs); !errors.Is(err, ErrUnsupportedPath) || !strings.Contains(err.Error(), gsha(0xcc).String()) {
		t.Errorf(".xylem path: %v (want ErrUnsupportedPath naming the commit)", err)
	}
	weird := []gitsource.Change{{Status: 'A', NewMode: 0o100600, NewSHA: gsha(1), Path: []byte("x")}}
	if _, err := MapCommit(commit(), weird, nil, fakeBlobs); err == nil {
		t.Error("unsupported mode accepted")
	}
	notUploaded := []gitsource.Change{{Status: 'A', NewMode: gitsource.ModeFile, NewSHA: gsha(1), Path: []byte("x")}}
	if _, err := MapCommit(commit(), notUploaded, nil, func(gitsource.SHA) (BlobInfo, bool) { return BlobInfo{}, false }); err == nil {
		t.Error("missing blob accepted")
	}
	if _, err := MapCommit(commit(gsha(1)), nil, nil, fakeBlobs); err == nil {
		t.Error("parent count mismatch accepted")
	}
}

// Every commit of the edge fixture maps to a changeset with a valid shape.
func TestMapEdgeFixture(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, err := gitsource.Open(ctx, testutil.GitFixture(t, "edge"), gitsource.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	tip, _ := src.ResolveBranch(ctx, "main")
	submoduleCommits := 0
	for sha, err := range src.RevList(ctx, tip) {
		if err != nil {
			t.Fatal(err)
		}
		c, err := src.ReadCommit(ctx, sha)
		if err != nil {
			t.Fatal(err)
		}
		var parent *gitsource.SHA
		if len(c.Parents) > 0 {
			parent = &c.Parents[0]
		}
		changes, err := src.DiffTree(ctx, parent, sha)
		if err != nil {
			t.Fatal(err)
		}
		parents := make([]hash.ID, len(c.Parents))
		for i, p := range c.Parents {
			parents[i] = hash.Sum(nil, p[:])
		}
		m, err := MapCommit(c, changes, parents, fakeBlobs)
		if err != nil {
			t.Fatalf("%s: %v", sha, err)
		}
		if len(m.Submodules) > 0 {
			submoduleCommits++
		}
		cs := changeset.Changeset{
			Parents: m.Request.Parents, Author: m.Request.Author, Committer: m.Committer,
			Message: m.Request.Message, Extras: m.Request.Extras, Changes: m.Request.Changes,
		}
		if err := cs.ValidateShape(changeset.ShapeOptions{AllowEmpty: true}); err != nil {
			t.Errorf("%s (%q): %v", sha, c.Message, err)
		}
	}
	if submoduleCommits != 3 {
		t.Errorf("%d commits touched the submodule, want 3 (add, move, remove)", submoduleCommits)
	}
}
