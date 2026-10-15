//go:build bench

// Package bench holds informational benchmarks (M8.6): many concurrent sparse
// clones through daemon → cache → server versus directly against the server.
package bench

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/cache"
	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/server/servertest"
	"github.com/delefrati/xylem/internal/testutil"
)

const (
	clones      = 100
	warmup      = 10
	maxInflight = 64 // the cache tier's upstream limit
)

var profiles = [][]string{{"internal/api"}, {"docs"}, {"pkg/client", "pkg/server"}, {"cmd/app", "internal/core"}}

// counter is a cache.Observer that also tracks the in-flight high-water mark.
type counter struct {
	hits, misses, inflight, maxInflight atomic.Int64
}

func (c *counter) Hit(n int)  { c.hits.Add(int64(n)) }
func (c *counter) Miss(n int) { c.misses.Add(int64(n)) }
func (c *counter) UpstreamInflight(d int) {
	v := c.inflight.Add(int64(d))
	for {
		m := c.maxInflight.Load()
		if v <= m || c.maxInflight.CompareAndSwap(m, v) {
			return
		}
	}
}

func (c *counter) reset() { c.hits.Store(0); c.misses.Store(0); c.maxInflight.Store(0) }

func dial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(cache.MaxMessageSize)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

type chain struct {
	daemon    string
	cacheObs  *counter
	daemonObs *counter
	stopCache func()
}

// startChain runs a cache tier (TCP) and a daemon (Unix socket) in process.
func startChain(t *testing.T, s *servertest.Server) chain {
	t.Helper()
	c := chain{cacheObs: &counter{}, daemonObs: &counter{}}
	serve := func(g *grpc.Server, network, addr string) string {
		lis, err := net.Listen(network, addr)
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = g.Serve(lis) }()
		t.Cleanup(g.Stop)
		return lis.Addr().String()
	}
	cacheTier := cache.NewTier(cache.Config{Upstream: xylemv1.NewObjectServiceClient(dial(t, s.Addr)), MaxInflight: maxInflight, Observe: c.cacheObs})
	cacheSrv := cache.NewService(cacheTier, cache.KindCache, nil).NewGRPCServer()
	cacheAddr := serve(cacheSrv, "tcp", "127.0.0.1:0")
	c.stopCache = cacheSrv.Stop

	daemonTier := cache.NewTier(cache.Config{Upstream: xylemv1.NewObjectServiceClient(dial(t, cacheAddr)), Observe: c.daemonObs})
	sock := filepath.Join(t.TempDir(), "d.sock")
	serve(cache.NewService(daemonTier, cache.KindDaemon, nil).NewGRPCServer(cache.Proxy(dial(t, s.Addr))...), "unix", sock)
	c.daemon = "unix://" + sock
	return c
}

type run struct {
	wall      time.Duration
	objects   int64 // objects the clients fetched
	errs      []error
	serverGet int
}

// cloneAll clones n random recent revisions with random sparse profiles.
// afterDone, if set, runs once each clone completes, with the number done.
func cloneAll(t *testing.T, s *servertest.Server, addr string, revs []string, n int, noCache bool, afterDone ...func(done int)) run {
	t.Helper()
	s.Store.Reset()
	var objects, done atomic.Int64
	var mu sync.Mutex
	var r run
	var wg sync.WaitGroup
	start := time.Now()
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir := filepath.Join(t.TempDir(), fmt.Sprintf("ws%03d", i))
			res, err := client.Clone(context.Background(), addr, dir, client.CloneOptions{
				Rev: "git:" + revs[i%len(revs)], Sparse: profiles[i%len(profiles)], NoCache: noCache,
			})
			defer func() {
				d := done.Add(1)
				for _, f := range afterDone {
					f(int(d))
				}
			}()
			if err != nil {
				mu.Lock()
				r.errs = append(r.errs, err)
				mu.Unlock()
				return
			}
			objects.Add(res.Fetched.Trees + res.Fetched.Blobs)
		}()
	}
	wg.Wait()
	r.wall, r.objects = time.Since(start), objects.Load()
	for _, n := range s.Store.Reads() {
		r.serverGet += n
	}
	return r
}

func TestTierBenchmark(t *testing.T) {
	s := servertest.Start(t, "branchy")
	revs := strings.Fields(testutil.Git(t, s.GitDir, "rev-list", "--first-parent", "-n", "10", "main"))

	direct := cloneAll(t, s, s.Addr, revs, clones, false)
	if len(direct.errs) > 0 {
		t.Fatalf("direct clones: %v", direct.errs[0])
	}

	c := startChain(t, s)
	warm := cloneAll(t, s, c.daemon, revs, warmup, true)
	if len(warm.errs) > 0 {
		t.Fatalf("warm-up clones: %v", warm.errs[0])
	}
	c.cacheObs.reset()
	c.daemonObs.reset()
	tiered := cloneAll(t, s, c.daemon, revs, clones, true)
	if len(tiered.errs) > 0 {
		t.Fatalf("tiered clones: %v", tiered.errs[0])
	}

	ratio := func(a, b int64) float64 {
		if b == 0 {
			return 0
		}
		return 100 * float64(a) / float64(b)
	}
	t.Logf("direct: %d clones in %v, %d objects requested, %d server reads", clones, direct.wall.Round(time.Millisecond), direct.objects, direct.serverGet)
	t.Logf("cold tiers (warm-up, %d clones): %d objects requested, %d server reads", warmup, warm.objects, warm.serverGet)
	t.Logf("warm tiers: %d clones in %v, %d objects requested, %d server reads (%.2f%%)", clones, tiered.wall.Round(time.Millisecond),
		tiered.objects, tiered.serverGet, ratio(int64(tiered.serverGet), tiered.objects))
	t.Logf("daemon hit rate %.1f%% (%d hits, %d misses); cache hit rate %.1f%% (%d hits, %d misses); cache upstream in flight ≤ %d (limit %d)",
		ratio(c.daemonObs.hits.Load(), c.daemonObs.hits.Load()+c.daemonObs.misses.Load()), c.daemonObs.hits.Load(), c.daemonObs.misses.Load(),
		ratio(c.cacheObs.hits.Load(), c.cacheObs.hits.Load()+c.cacheObs.misses.Load()), c.cacheObs.hits.Load(), c.cacheObs.misses.Load(),
		c.cacheObs.maxInflight.Load(), maxInflight)
	if pct := ratio(int64(tiered.serverGet), tiered.objects); pct >= 5 {
		t.Errorf("server reads are %.2f%% of object requests after warm-up, want < 5%%", pct)
	}

	// Kill the cache tier during a fresh round (new daemon, cold): clones fail
	// cleanly with Unavailable, and the cache never exceeded its limit.
	c2 := startChain(t, s)
	var kill sync.Once
	killed := cloneAll(t, s, c2.daemon, revs, clones, true, func(done int) {
		if done == 10 {
			kill.Do(func() { go c2.stopCache() })
		}
	})
	unavailable := 0
	for _, err := range killed.errs {
		if status.Code(err) == codes.Unavailable {
			unavailable++
		} else {
			t.Errorf("clone with the cache killed: %v (code %v), want Unavailable", err, status.Code(err))
		}
	}
	t.Logf("cache killed mid-run: %d of %d clones failed, %d with Unavailable; cache upstream in flight ≤ %d (limit %d)",
		len(killed.errs), clones, unavailable, c2.cacheObs.maxInflight.Load(), maxInflight)
	if c2.cacheObs.maxInflight.Load() > maxInflight || c.cacheObs.maxInflight.Load() > maxInflight {
		t.Errorf("cache upstream in flight exceeded %d", maxInflight)
	}
}
