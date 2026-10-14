//go:build integration

package server_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
)

func TestChangesetAndRefRPCs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := startServer(t)
	r := c.Env.Repo
	root := repotest.Commit(t, r, nil, "root", repotest.Put(t, r, "a", "1"), repotest.Put(t, r, "b", "2"))
	child, _, err := r.CreateChangeset(ctx, repo.CreateRequest{
		Parents: []hash.ID{root.ID}, Author: repotest.Author, Message: []byte("child\n"),
		Extras:  map[string][]byte{changeset.ExtraGitSHA: []byte(strings.Repeat("ab", 20))},
		Changes: []changeset.Change{repotest.Delete("b"), repotest.Put(t, r, "c", "3")},
	}, repo.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	draft := repotest.Commit(t, r, []hash.ID{child.ID}, "draft", repotest.Put(t, r, "d", "4"))
	if _, err := r.UpdateRef(ctx, repo.MainRef, hash.ID{}, child.ID, false, "t", "admin"); err != nil {
		t.Fatal(err)
	}

	// ResolveRevision.
	for rev, want := range map[string]*repo.Changeset{"main": child, "main~1": root, "main^": root, "git:abab": child, draft.ID.Short(10): draft} {
		resp, err := c.Repo.ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Revision: rev})
		if err != nil || string(resp.Id) != string(want.ID[:]) || string(resp.RootTree) != string(want.Core.RootTree[:]) {
			t.Errorf("ResolveRevision(%q) = %x, %v", rev, resp.GetId(), err)
		}
	}
	for rev, code := range map[string]codes.Code{"": codes.InvalidArgument, "nope": codes.NotFound, "main~9": codes.NotFound, "@": codes.InvalidArgument, "Bad Rev": codes.InvalidArgument} {
		if _, err := c.Repo.ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Revision: rev}); status.Code(err) != code {
			t.Errorf("ResolveRevision(%q) = %v, want %v", rev, err, code)
		}
	}

	// GetChangesets: request order, include_changes, on_mainline.
	resp, err := c.Repo.GetChangesets(ctx, &xylemv1.GetChangesetsRequest{Ids: [][]byte{draft.ID[:], root.ID[:], child.ID[:]}, IncludeChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Changesets
	if len(got) != 3 || string(got[0].Id) != string(draft.ID[:]) || string(got[1].Id) != string(root.ID[:]) {
		t.Fatalf("order: %v", got)
	}
	if got[0].OnMainline || !got[1].OnMainline || !got[2].OnMainline {
		t.Errorf("on_mainline = %v %v %v, want false true true", got[0].OnMainline, got[1].OnMainline, got[2].OnMainline)
	}
	ch := got[2]
	if ch.GitSha != strings.Repeat("ab", 20) || ch.ChangeCount != 2 || len(ch.Changes) != 2 || ch.Changes[0].Op != xylemv1.FileChange_OP_DELETE ||
		string(ch.Message) != "child\n" || ch.Generation != 2 || ch.FirstParentDepth != 1 || string(ch.Parents[0]) != string(root.ID[:]) {
		t.Errorf("child changeset = %v", ch)
	}
	if ch.Author.Time.Seconds != repotest.Author.Time || string(ch.Author.NameEmail) != string(repotest.Author.NameEmail) {
		t.Errorf("author = %v", ch.Author)
	}
	noChanges, _ := c.Repo.GetChangesets(ctx, &xylemv1.GetChangesetsRequest{Ids: [][]byte{child.ID[:]}})
	if len(noChanges.Changesets[0].Changes) != 0 || noChanges.Changesets[0].ChangeCount != 2 {
		t.Error("changes returned without include_changes")
	}
	if _, err := c.Repo.GetChangesets(ctx, &xylemv1.GetChangesetsRequest{Ids: [][]byte{make([]byte, 32)}}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown id: %v", err)
	}
	if _, err := c.Repo.GetChangesets(ctx, &xylemv1.GetChangesetsRequest{Ids: [][]byte{{1}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("short id: %v", err)
	}
	if _, err := c.Repo.GetChangesets(ctx, &xylemv1.GetChangesetsRequest{Ids: make([][]byte, 1001)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("1001 ids: %v", err)
	}

	// UpdateRef.
	created, err := c.Repo.UpdateRef(ctx, &xylemv1.UpdateRefRequest{Name: "release/1", NewId: root.ID[:]})
	if err != nil || created.Ref.Version != 1 {
		t.Fatalf("create ref: %v, %v", created, err)
	}
	if _, err := c.Repo.UpdateRef(ctx, &xylemv1.UpdateRefRequest{Name: "main", ExpectedId: child.ID[:], NewId: draft.ID[:]}); err != nil {
		t.Errorf("fast-forward: %v", err)
	}
	if _, err := c.Repo.UpdateRef(ctx, &xylemv1.UpdateRefRequest{Name: "main", ExpectedId: child.ID[:], NewId: draft.ID[:]}); status.Code(err) != codes.Aborted {
		t.Errorf("stale expected: %v", err)
	}
	if _, err := c.Repo.UpdateRef(ctx, &xylemv1.UpdateRefRequest{Name: "main", ExpectedId: draft.ID[:], NewId: root.ID[:]}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("non-fast-forward: %v", err)
	}
	if forced, err := c.Repo.UpdateRef(ctx, &xylemv1.UpdateRefRequest{Name: "main", ExpectedId: draft.ID[:], NewId: root.ID[:], Force: true}); err != nil || forced.Ref.Version != 3 {
		t.Errorf("forced: %v, %v", forced, err)
	}
	if _, err := c.Repo.UpdateRef(ctx, &xylemv1.UpdateRefRequest{Name: "BAD", NewId: root.ID[:]}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad name: %v", err)
	}
	if _, err := c.Repo.UpdateRef(ctx, &xylemv1.UpdateRefRequest{Name: "x", NewId: []byte{1}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad id: %v", err)
	}
}
