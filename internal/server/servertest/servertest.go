// Package servertest serves an imported git fixture on a loopback port for
// client and end-to-end tests.
package servertest

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/importer"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/testutil"
)

// CountingStore records which objects were read from the store.
type CountingStore struct {
	store.ObjectStore
	mu   sync.Mutex
	read map[hash.ID]int
}

// Get implements store.ObjectStore and records the read.
func (c *CountingStore) Get(ctx context.Context, id hash.ID) ([]byte, error) {
	c.mu.Lock()
	c.read[id]++
	c.mu.Unlock()
	return c.ObjectStore.Get(ctx, id)
}

// Reads returns the distinct object ids read so far.
func (c *CountingStore) Reads() map[hash.ID]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[hash.ID]int, len(c.read))
	for k, v := range c.read {
		out[k] = v
	}
	return out
}

// Reset forgets recorded reads.
func (c *CountingStore) Reset() {
	c.mu.Lock()
	c.read = map[hash.ID]int{}
	c.mu.Unlock()
}

// Server is a running test server.
type Server struct {
	Addr   string
	Env    *repotest.Env
	Store  *CountingStore
	GitDir string
	Git    *gitsource.Repo
	TipID  hash.ID // the imported branch tip
}

// Start imports fixture profile ("" for an empty repository) into a fresh
// database and serves it. The server's object cache is off, so every object
// a client fetches is one recorded store read. tweak adjusts the server's
// config (auth, for example).
func Start(t testing.TB, profile string, tweak ...func(*server.Config)) *Server {
	t.Helper()
	ctx := context.Background()
	cs := &CountingStore{ObjectStore: store.NewMem(), read: map[hash.ID]int{}}
	env := repotest.NewEnv(t, cs)
	s := &Server{Env: env, Store: cs}
	if profile != "" {
		s.GitDir = testutil.GitFixture(t, profile)
		src, err := gitsource.Open(ctx, s.GitDir, gitsource.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = src.Close() })
		s.Git = src
		im, err := importer.New(env.Repo, src, importer.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := im.Run(ctx); err != nil {
			t.Fatal(err)
		}
		main, err := env.Repo.GetRef(ctx, repo.MainRef)
		if err != nil {
			t.Fatal(err)
		}
		s.TipID = main.ID
	}
	cs.Reset() // only count client reads
	cfg := server.Config{Meta: env.Meta, Objects: cs}
	for _, f := range tweak {
		f(&cfg)
	}
	g := server.New(cfg).NewGRPCServer()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	s.Addr = lis.Addr().String()
	return s
}
