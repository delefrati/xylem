//go:build integration

package client_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sync/errgroup"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"

	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/server/servertest"
)

type ws struct {
	t *testing.T
	w *client.Workspace
	f *client.Fetcher
}

func cloneWS(t *testing.T, s *servertest.Server, name string) *ws {
	t.Helper()
	res, err := client.Clone(context.Background(), s.Addr, filepath.Join(t.TempDir(), name), client.CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return &ws{t, res.Workspace, client.NewFetcher(dial(t, s.Addr), "default", client.NewObjectCache(res.Workspace.Meta("objects"), 0))}
}

func (x *ws) write(rel, content string) { writeFile(x.t, x.w.Root, rel, content) }

func (x *ws) commit(msg string) {
	x.t.Helper()
	if _, err := x.w.Commit(context.Background(), x.f, client.CommitOptions{Message: []byte(msg + "\n"), All: true, Author: testAuthor}); err != nil {
		x.t.Fatalf("commit %q: %v", msg, err)
	}
}

func (x *ws) subjects() []string {
	x.t.Helper()
	stack, err := x.w.Stack(context.Background(), x.f)
	if err != nil {
		x.t.Fatal(err)
	}
	out := []string{}
	for _, c := range stack {
		out = append(out, c.Subject())
	}
	return out
}

func (x *ws) read(rel string) string {
	b, err := os.ReadFile(filepath.Join(x.w.Root, rel))
	if err != nil {
		x.t.Fatal(err)
	}
	return string(b)
}

func TestStackSyncLand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := servertest.Start(t, "tiny")
	a, b := cloneWS(t, s, "a"), cloneWS(t, s, "b")

	if _, err := a.w.Land(ctx, a.f); err == nil {
		t.Error("land with an empty stack succeeded")
	}
	a.write("a1.txt", "a1\n")
	a.commit("a1")
	a.write("a2.txt", "a2\n")
	a.commit("a2")
	if diff := cmp.Diff([]string{"a1", "a2"}, a.subjects()); diff != "" {
		t.Fatalf("stack (-want +got):\n%s", diff)
	}

	// B lands a change to another file first.
	b.write("b.txt", "b\n")
	b.commit("b1")
	res, err := b.w.Land(ctx, b.f)
	if err != nil || len(res.Moved) != 1 || res.Attempts != 1 || b.w.State.Parent != res.Top {
		t.Fatalf("b land = %+v, %v", res, err)
	}
	if got := b.subjects(); len(got) != 0 {
		t.Errorf("b stack after land %q", got)
	}
	if res.Checkout.Fetched.Blobs != 0 { // the landed tip only has b's own new blob
		t.Errorf("b fetched %d blobs after land, want 0", res.Checkout.Fetched.Blobs)
	}

	// A syncs: its drafts are rebased onto main, which has b.txt.
	res, err = a.w.Sync(ctx, a.f)
	if err != nil || len(res.Moved) != 2 || a.w.State.Parent != res.Top {
		t.Fatalf("a sync = %+v, %v", res, err)
	}
	if a.read("b.txt") != "b\n" || a.read("a2.txt") != "a2\n" {
		t.Error("a's working copy after sync")
	}
	if res.Checkout.Fetched.Blobs != 1 { // b.txt only
		t.Errorf("a fetched %d blobs on sync, want 1", res.Checkout.Fetched.Blobs)
	}
	if diff := cmp.Diff([]string{"a1", "a2"}, a.subjects()); diff != "" {
		t.Errorf("stack after sync (-want +got):\n%s", diff)
	}
	if got := statusOf(t, a.w, a.f); len(got) != 0 {
		t.Errorf("status after sync %q", got)
	}
	if res, err = a.w.Land(ctx, a.f); err != nil || len(res.Moved) != 2 {
		t.Fatalf("a land = %+v, %v", res, err)
	}

	// B is behind: sync with no drafts checks out main.
	if res, err = b.w.Sync(ctx, b.f); err != nil || res.Top != a.w.State.Parent || b.read("a1.txt") != "a1\n" {
		t.Fatalf("b sync = %+v, %v", res, err)
	}

	// Both edit README; B lands first, A's land conflicts and changes nothing.
	a.write("README", "a's readme\n")
	a.commit("a edits README")
	b.write("README", "b's readme\n")
	b.commit("b edits README")
	if _, err := b.w.Land(ctx, b.f); err != nil {
		t.Fatal(err)
	}
	before := a.w.State
	_, err = a.w.Land(ctx, a.f)
	var conflict *client.ConflictError
	if client.ExitCode(err) != client.ExitConflict || !errors.As(err, &conflict) || len(conflict.Conflicts) != 1 ||
		string(conflict.Conflicts[0].Path) != "README" || conflict.Conflicts[0].Kind != "content" {
		t.Fatalf("conflicting land: %v", err)
	}
	if a.w.State.Parent != before.Parent || a.read("README") != "a's readme\n" {
		t.Error("a conflicting land changed the workspace")
	}
	if _, err := a.w.Sync(ctx, a.f); client.ExitCode(err) != client.ExitConflict {
		t.Errorf("conflicting sync: %v", err)
	}

	// Local changes refuse sync and land.
	b.write("b.txt", "dirty\n")
	if _, err := b.w.Sync(ctx, b.f); client.ExitCode(err) != client.ExitDirty {
		t.Errorf("dirty sync: %v", err)
	}
	if _, err := b.w.Land(ctx, b.f); client.ExitCode(err) != client.ExitDirty {
		t.Errorf("dirty land: %v", err)
	}
}

// TestTwentyWorkspacesLand is the MVP check (01 §MVP is done when): twenty
// workspaces each commit a non-conflicting change and land at the same time;
// all land and main stays a valid first-parent chain with all twenty.
func TestTwentyWorkspacesLand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := servertest.Start(t, "tiny")
	const n = 20
	wss := make([]*ws, n)
	for i := range wss {
		wss[i] = cloneWS(t, s, fmt.Sprintf("w%02d", i))
		wss[i].write(fmt.Sprintf("work/w%02d.txt", i), fmt.Sprintf("workspace %d\n", i))
		wss[i].commit(fmt.Sprintf("w%02d", i))
	}
	start := make(chan struct{})
	var g errgroup.Group
	attempts := make([]int, n)
	for i, x := range wss {
		g.Go(func() error {
			<-start
			res, err := x.w.Land(ctx, x.f)
			if err != nil {
				return fmt.Errorf("w%02d: %w", i, err)
			}
			attempts[i] = res.Attempts
			return nil
		})
	}
	close(start)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	t.Logf("CAS attempts per land: %v", attempts)

	// main's first-parent chain has the twenty lands on top of the import.
	conn := dial(t, s.Addr)
	resp, err := conn.Repo.Log(ctx, &xylemv1.LogRequest{Revision: "main", Page: &xylemv1.PageRequest{PageSize: n + 1}})
	if err != nil {
		t.Fatal(err)
	}
	subjects := map[string]bool{}
	for i, c := range resp.GetChangesets()[:n] {
		subjects[string(c.GetMessage())] = true
		if !c.GetOnMainline() || c.GetFirstParentDepth() != resp.GetChangesets()[n].GetFirstParentDepth()+uint64(n-i) {
			t.Errorf("changeset %d of the log: depth %d, on mainline %v", i, c.GetFirstParentDepth(), c.GetOnMainline())
		}
	}
	if len(subjects) != n || string(resp.GetChangesets()[n].GetId()) != string(s.TipID[:]) {
		t.Errorf("main has %d distinct lands above the imported tip", len(subjects))
	}
	// A sync brings every workspace's file to the first one.
	if _, err := wss[0].w.Sync(ctx, wss[0].f); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if got := wss[0].read(fmt.Sprintf("work/w%02d.txt", i)); got != fmt.Sprintf("workspace %d\n", i) {
			t.Errorf("work/w%02d.txt = %q", i, got)
		}
	}
}
