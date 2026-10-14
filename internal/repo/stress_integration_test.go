//go:build integration

package repo_test

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
)

// branch tracks a tip and its flat file map so generated changes are valid.
type branch struct {
	tip   *repo.Changeset
	files map[string]string // path → content
}

type stressRNG struct{ src *rand.ChaCha8 }

func (r stressRNG) intn(n int) int { return int(r.src.Uint64()>>34) % n } // n is small

// TestStress (M2.9): 10,000 changesets on a 1,000-file tree with random puts
// and deletes, ~5% two-parent merges, main advanced in fast-forward steps,
// then a full verify that must find nothing. Under -race it runs 1,000.
func TestStress(t *testing.T) {
	total := 10_000
	if raceEnabled {
		total = 1_000
	}
	ctx := context.Background()
	r := repotest.New(t)
	rng := stressRNG{rand.NewChaCha8([32]byte{42})}
	content := 0
	newContent := func() string { content++; return fmt.Sprint("content #", content) }

	dirs := make([]string, 0, 100)
	for d := range 20 {
		for s := range 5 {
			dirs = append(dirs, fmt.Sprintf("dir%02d/sub%d", d, s))
		}
	}
	main := &branch{files: map[string]string{}}
	var initial []changeset.Change
	for i := range 1000 {
		p := fmt.Sprintf("%s/file%04d.txt", dirs[i%len(dirs)], i)
		c := newContent()
		main.files[p] = c
		initial = append(initial, repotest.Put(t, r, p, c))
	}
	start := time.Now()
	main.tip = repotest.Commit(t, r, nil, "1,000 files", initial...)
	if _, err := r.UpdateRef(ctx, repo.MainRef, hash.ID{}, main.tip.ID, false, "stress", "admin"); err != nil {
		t.Fatal(err)
	}
	mainRef := main.tip.ID

	// changes returns 1–10 random valid changes for b.
	changes := func(b *branch) ([]changeset.Change, map[string]string) {
		next := maps.Clone(b.files)
		existing := slices.Sorted(maps.Keys(b.files))
		touched := map[string]bool{}
		var out []changeset.Change
		for range 1 + rng.intn(10) {
			switch op := rng.intn(10); {
			case op < 3 && len(next) > 500: // delete (30%, balancing adds)
				p := existing[rng.intn(len(existing))]
				if touched[p] || next[p] == "" {
					continue
				}
				touched[p] = true
				delete(next, p)
				out = append(out, repotest.Delete(p))
			case op < 7: // modify (40%)
				p := existing[rng.intn(len(existing))]
				if touched[p] || next[p] == "" {
					continue
				}
				touched[p] = true
				c := newContent()
				next[p] = c
				out = append(out, repotest.Put(t, r, p, c))
			default: // add (30%)
				p := fmt.Sprintf("%s/new%06d.txt", dirs[rng.intn(len(dirs))], content)
				c := newContent()
				touched[p] = true
				next[p] = c
				out = append(out, repotest.Put(t, r, p, c))
			}
		}
		return out, next
	}
	commit := func(b *branch, parents []hash.ID, msg string) {
		var ch []changeset.Change
		var next map[string]string
		for len(ch) == 0 {
			ch, next = changes(b)
		}
		cs, _, err := r.CreateChangeset(ctx, repo.CreateRequest{Parents: parents, Author: repotest.Author, Message: []byte(msg), Changes: ch}, repo.CreateOptions{})
		if err != nil {
			t.Fatalf("%s: %v", msg, err)
		}
		b.tip, b.files = cs, next
	}

	var side *branch
	merges, sideCommits, ffSteps := 0, 0, 0
	for n := 1; n < total; n++ {
		msg := fmt.Sprint("change ", n)
		switch {
		case side != nil && rng.intn(13) == 0: // merges end up ~5% of all changesets
			commit(main, []hash.ID{main.tip.ID, side.tip.ID}, "merge "+msg)
			side = nil
			merges++
		case side == nil && rng.intn(5) == 0: // start a side branch at the current tip
			side = &branch{tip: main.tip, files: maps.Clone(main.files)}
			commit(side, []hash.ID{side.tip.ID}, "side "+msg)
			sideCommits++
		case side != nil && rng.intn(3) == 0:
			commit(side, []hash.ID{side.tip.ID}, "side "+msg)
			sideCommits++
		default:
			commit(main, []hash.ID{main.tip.ID}, msg)
		}
		if rng.intn(8) == 0 && main.tip.ID != mainRef { // fast-forward main
			if _, err := r.UpdateRef(ctx, repo.MainRef, mainRef, main.tip.ID, false, "stress", "land"); err != nil {
				t.Fatalf("fast-forward %d: %v", n, err)
			}
			mainRef = main.tip.ID
			ffSteps++
		}
	}
	if main.tip.ID != mainRef {
		if _, err := r.UpdateRef(ctx, repo.MainRef, mainRef, main.tip.ID, false, "stress", "land"); err != nil {
			t.Fatal(err)
		}
		ffSteps++
	}
	created := time.Since(start)

	verifyStart := time.Now()
	report, err := r.Verify(ctx, repo.VerifyOptions{Objects: true})
	if err != nil {
		t.Fatal(err)
	}
	verified := time.Since(verifyStart)
	if report.Changesets != total || len(report.Violations) != 0 {
		var lines []string
		for _, v := range report.Violations {
			lines = append(lines, v.String())
		}
		t.Fatalf("verify: %d changesets checked (want %d), %d violations:\n%s", report.Changesets, total, len(report.Violations), strings.Join(lines, "\n"))
	}
	t.Logf("stress: %d changesets (%d merges = %.1f%%, %d side commits), main fast-forwarded %d times, main tree %d files",
		total, merges, 100*float64(merges)/float64(total), sideCommits, ffSteps, len(main.files))
	t.Logf("stress: created in %v (%.2f ms/changeset); verify --all --objects in %v; 0 violations",
		created.Round(time.Millisecond), float64(created.Microseconds())/1000/float64(total), verified.Round(time.Millisecond))
}
