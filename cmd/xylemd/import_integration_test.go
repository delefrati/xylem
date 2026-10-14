//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/testutil"
)

// syncBuffer is a bytes.Buffer safe for a writer and a concurrent reader.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestImportCommand(t *testing.T) {
	pool := testutil.NewDB(t)
	storeDir := t.TempDir()
	gitDir := testutil.CopyFixture(t, "tiny")
	flags := []string{"--db", pool.Config().ConnString(), "--store", "fs://" + storeDir, "--git", gitDir}

	out, err := run(t, append([]string{"import"}, flags...)...)
	if err != nil || !strings.Contains(out, "imported 10 new commits") {
		t.Fatalf("import: %q, %v", out, err)
	}
	if out, err := run(t, append([]string{"import"}, flags...)...); err != nil || !strings.Contains(out, "imported 0 new commits") {
		t.Fatalf("re-import: %q, %v", out, err)
	}
	if _, err := run(t, "import", "--db", pool.Config().ConnString()); err == nil {
		t.Error("import without --git succeeded")
	}

	// --follow picks up a commit made while it is polling.
	ctx, cancel := context.WithCancel(context.Background())
	var out2 syncBuffer
	done := make(chan error, 1)
	go func() {
		root := newRootCmd()
		root.SetOut(&out2)
		root.SetErr(&out2)
		root.SetArgs(append([]string{"import", "--follow", "100ms", "--log-level", "warn"}, flags...))
		done <- root.ExecuteContext(ctx)
	}()
	testutil.Git(t, gitDir, "commit", "-q", "--allow-empty", "-m", "made while following")
	newTip := testutil.Git(t, gitDir, "rev-parse", "main")

	r, err := repo.Open(context.Background(), meta.NewPG(pool), store.NewFS(storeDir), "default", repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		m, err := r.GitSHAMap(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var imported bool
		for sha := range m {
			if strings.EqualFold(newTip, hexOf(sha)) {
				imported = true
			}
		}
		if imported {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("--follow did not import the new commit; output:\n%s", out2.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("--follow stopped with %v", err)
	}
	out, err = run(t, append([]string{"verify", "default", "--all", "--objects"}, flags[:4]...)...)
	if err != nil || !strings.Contains(out, "checked 11 changesets and 1 refs: 0 violations") {
		t.Errorf("verify after follow: %q, %v", out, err)
	}

	// verify-import: all, sample, since, argument errors, and a mismatch.
	if out, err := run(t, append([]string{"verify-import", "--all"}, flags...)...); err != nil || !strings.Contains(out, "compared 11 commits") || !strings.Contains(out, "0 mismatches") {
		t.Errorf("verify-import --all: %q, %v", out, err)
	}
	if out, err := run(t, append([]string{"verify-import", "--sample", "3", "--seed", "9"}, flags...)...); err != nil || !strings.Contains(out, "compared 3 commits") {
		t.Errorf("verify-import --sample: %q, %v", out, err)
	}
	prev := testutil.Git(t, gitDir, "rev-parse", "main~1")
	if out, err := run(t, append([]string{"verify-import", "--all", "--since", prev}, flags...)...); err != nil || !strings.Contains(out, "compared 1 commits") {
		t.Errorf("verify-import --since: %q, %v", out, err)
	}
	if _, err := run(t, append([]string{"verify-import"}, flags...)...); err == nil {
		t.Error("verify-import without --all or --sample succeeded")
	}
	testutil.Git(t, gitDir, "commit", "-q", "--allow-empty", "-m", "not imported")
	if out, err := run(t, append([]string{"verify-import", "--all"}, flags...)...); err == nil || !strings.Contains(out, "MISMATCH") {
		t.Errorf("verify-import with an unimported commit: %q, %v", out, err)
	}
}

func hexOf(sha [20]byte) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	for _, c := range sha {
		b.WriteByte(digits[c>>4])
		b.WriteByte(digits[c&15])
	}
	return b.String()
}
