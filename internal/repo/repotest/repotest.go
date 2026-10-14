// Package repotest builds repo.Repo instances for tests: a fresh Postgres
// database (testutil.NewDB) and an in-memory object store. It is a separate
// package so that testutil does not import repo.
package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/testutil"
)

// Clock is the fixed time repos built here use: t0 = 1700000000.
var Clock = func() time.Time { return time.Unix(1_700_000_000, 0) }

// New returns a Repo named "default" on a fresh database and a Mem store,
// with Now fixed to Clock.
func New(t testing.TB) *repo.Repo {
	t.Helper()
	r, _ := NewWithMeta(t)
	return r
}

// NewWithMeta is New that also returns the meta store.
func NewWithMeta(t testing.TB) (*repo.Repo, *meta.PG) {
	t.Helper()
	ctx := context.Background()
	m := meta.NewPG(testutil.NewDB(t))
	if _, err := m.CreateRepo(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(ctx, m, store.NewMem(), "default", repo.Options{Now: Clock})
	if err != nil {
		t.Fatal(err)
	}
	return r, m
}

// Put stores content as a blob in r's object store and returns a PUT change.
func Put(t testing.TB, r *repo.Repo, path, content string) changeset.Change {
	t.Helper()
	id, stored := object.Encode(object.KindBlob, []byte(content))
	if err := r.Objects().Put(context.Background(), id, stored); err != nil {
		t.Fatal(err)
	}
	return changeset.Change{Path: []byte(path), Op: changeset.OpPut, Mode: object.ModeFile, Size: uint64(len(content)), Blob: id}
}

// Delete returns a DELETE change.
func Delete(path string) changeset.Change {
	return changeset.Change{Path: []byte(path), Op: changeset.OpDelete}
}

// Author is the signature test changesets use.
var Author = changeset.Signature{NameEmail: []byte("Test <test@example.com>"), Time: 1_700_000_000}

// Commit creates a changeset with the given parents and changes, failing the
// test on error.
func Commit(t testing.TB, r *repo.Repo, parents []hash.ID, msg string, changes ...changeset.Change) *repo.Changeset {
	t.Helper()
	cs, _, err := r.CreateChangeset(context.Background(), repo.CreateRequest{
		Parents: parents, Author: Author, Message: []byte(msg), Changes: changes,
	}, repo.CreateOptions{AllowRoot: len(parents) == 0})
	if err != nil {
		t.Fatalf("CreateChangeset(%q): %v", msg, err)
	}
	return cs
}
