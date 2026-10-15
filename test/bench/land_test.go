//go:build bench

package bench

import (
	"context"
	"fmt"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/server/servertest"
)

const (
	landers        = 50
	landsPerLander = 20
	landWindow     = 20 * time.Millisecond // 07 §Contention mitigation
)

type landRun struct {
	elapsed  time.Duration
	attempts int
}

func (r landRun) perSecond() float64 { return landers * landsPerLander / r.elapsed.Seconds() }

// runLands serves a fresh repository (batch window 0 = off) and lands
// landers × landsPerLander single-changeset stacks, all based on the root so
// every land rebases, from landers concurrent gRPC clients. Only the lands
// are timed.
func runLands(t *testing.T, window time.Duration) landRun {
	t.Helper()
	ctx := context.Background()
	s := servertest.Start(t, "", func(cfg *server.Config) { cfg.RepoOptions.LandBatchWindow = window })
	r := s.Env.Repo
	root := repotest.Commit(t, r, nil, "root", repotest.Put(t, r, "README", "r\n"))
	if _, err := r.UpdateRef(ctx, repo.MainRef, hash.ID{}, root.ID, false, "bench", "admin"); err != nil {
		t.Fatal(err)
	}
	stacks := make([][]hash.ID, landers)
	for w := range stacks {
		for i := range landsPerLander {
			path := fmt.Sprintf("w%02d/f%02d.txt", w, i)
			id, stored := object.Encode(object.KindBlob, []byte(path))
			if err := r.Objects().Put(ctx, id, stored); err != nil {
				t.Fatal(err)
			}
			cs, _, err := r.CreateChangeset(ctx, repo.CreateRequest{
				Parents: []hash.ID{root.ID}, Author: repotest.Author, Message: []byte(path),
				Changes: []changeset.Change{{Path: []byte(path), Op: changeset.OpPut, Mode: object.ModeFile, Size: uint64(len(path)), Blob: id}},
			}, repo.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			stacks[w] = append(stacks[w], cs.ID)
		}
	}
	conns := make([]xylemv1.WriteServiceClient, landers)
	for i := range conns {
		cc, err := grpc.NewClient(s.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cc.Close() })
		conns[i] = xylemv1.NewWriteServiceClient(cc)
	}

	attempts := make([]int, landers)
	start := time.Now()
	var g errgroup.Group
	for w := range landers {
		g.Go(func() error {
			for _, id := range stacks[w] {
				resp, err := conns[w].Land(ctx, &xylemv1.LandRequest{Stack: [][]byte{id[:]}, MaxAttempts: server.MaxLandAttempts})
				if err != nil {
					return err
				}
				if len(resp.GetLanded()) != 1 {
					return fmt.Errorf("land: %v", resp)
				}
				attempts[w] += int(resp.GetAttempts())
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	run := landRun{elapsed: time.Since(start)}
	for _, a := range attempts {
		run.attempts += a
	}
	tip, err := r.GetRef(ctx, repo.MainRef)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := r.Changeset(ctx, tip.ID)
	if err != nil || cs.Core.FPDepth-root.Core.FPDepth != landers*landsPerLander {
		t.Fatalf("main is %d first-parent steps from the root, want %d (%v)", cs.Core.FPDepth-root.Core.FPDepth, landers*landsPerLander, err)
	}
	return run
}

// TestLandBenchmark is M9.3's benchmark: 50 concurrent landers through the
// gRPC server, the land batcher off vs on. Acceptance: on is ≥ 3× faster.
func TestLandBenchmark(t *testing.T) {
	off := runLands(t, 0)
	on := runLands(t, landWindow)
	n := landers * landsPerLander
	t.Logf("%d landers × %d lands, every land rebasing:", landers, landsPerLander)
	t.Logf("  batcher off:        %6.0f lands/s (%v, %.2f CAS attempts per land)", off.perSecond(), off.elapsed.Round(time.Millisecond), float64(off.attempts)/float64(n))
	t.Logf("  batcher on (%v): %6.0f lands/s (%v, %.2f CAS attempts per land)", landWindow, on.perSecond(), on.elapsed.Round(time.Millisecond), float64(on.attempts)/float64(n))
	ratio := on.perSecond() / off.perSecond()
	t.Logf("  speedup: %.1f×", ratio)
	if ratio < 3 {
		t.Errorf("the batcher is %.1f× faster at %d concurrent landers, want ≥ 3×", ratio, landers)
	}
}
