package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
)

// Fetch limits (09 §Client algorithm).
const (
	fetchBatch    = 1024
	fetchParallel = 8
)

// FileEntry is a file of a checked-out tree.
type FileEntry struct {
	Path []byte
	Mode object.Mode
	ID   hash.ID
	Size uint64
}

// FetchStats counts objects fetched from the server.
type FetchStats struct {
	Trees, Blobs int64
	Bytes        int64
}

// Fetcher brings trees and blobs into the local cache.
type Fetcher struct {
	conn     *Conn
	repo     string
	cache    *ObjectCache
	progress io.Writer  // nil: no progress
	mu       sync.Mutex // serializes progress output

	trees, blobs, bytes atomic.Int64
}

// NewFetcher returns a fetcher. Progress is written to stderr when it is a
// terminal.
func NewFetcher(conn *Conn, repo string, cache *ObjectCache) *Fetcher {
	f := &Fetcher{conn: conn, repo: repo, cache: cache}
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		f.progress = os.Stderr
	}
	return f
}

// Conn returns the fetcher's server connection.
func (f *Fetcher) Conn() *Conn { return f.conn }

// Stats returns what has been fetched so far.
func (f *Fetcher) Stats() FetchStats {
	return FetchStats{Trees: f.trees.Load(), Blobs: f.blobs.Load(), Bytes: f.bytes.Load()}
}

// Fetch brings every tree on the way to and inside the scope, then every
// in-scope blob, into the cache, and returns the in-scope files in
// depth-first path order.
func (f *Fetcher) Fetch(ctx context.Context, root hash.ID, scope Scope) ([]FileEntry, error) {
	type dir struct {
		path []byte
		id   hash.ID
	}
	level := []dir{{nil, root}}
	var files []FileEntry
	for len(level) > 0 {
		ids := make([]hash.ID, len(level))
		for i, d := range level {
			ids[i] = d.id
		}
		if err := f.ensure(ctx, ids, &f.trees); err != nil {
			return nil, err
		}
		var next []dir
		for _, d := range level {
			tree, err := f.readTree(d.id)
			if err != nil {
				return nil, err
			}
			for _, e := range tree.Entries {
				p := e.Name
				if len(d.path) > 0 {
					p = paths.Join(d.path, e.Name)
				}
				switch {
				case e.Mode == object.ModeTree:
					if scope.TreeInScope(p) {
						next = append(next, dir{p, e.ID})
					}
				case scope.FileInScope(p):
					files = append(files, FileEntry{Path: p, Mode: e.Mode, ID: e.ID, Size: e.Size})
				}
			}
		}
		level = next
	}
	blobs := make([]hash.ID, len(files))
	for i, fe := range files {
		blobs[i] = fe.ID
	}
	if err := f.ensure(ctx, blobs, &f.blobs); err != nil {
		return nil, err
	}
	f.done()
	return sortFiles(files), nil
}

func (f *Fetcher) readTree(id hash.ID) (*object.Tree, error) {
	kind, body, err := f.cache.GetVerified(id)
	if err != nil {
		return nil, err
	}
	if kind != object.KindTree {
		return nil, fmt.Errorf("object %s is a %v, not a tree", id.Short(12), kind)
	}
	return object.DecodeTree(body)
}

// Ensure fetches the objects among ids that are not cached yet.
func (f *Fetcher) Ensure(ctx context.Context, ids []hash.ID) error {
	var n atomic.Int64
	defer func() { f.blobs.Add(n.Load()) }()
	return f.ensure(ctx, ids, &n)
}

// ensure fetches missing ids in batches over up to fetchParallel streams.
func (f *Fetcher) ensure(ctx context.Context, ids []hash.ID, counter *atomic.Int64) error {
	seen := make(map[hash.ID]bool, len(ids))
	var missing []hash.ID
	for _, id := range ids {
		if !seen[id] && !f.cache.Has(id) {
			missing = append(missing, id)
		}
		seen[id] = true
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(fetchParallel)
	for start := 0; start < len(missing); start += fetchBatch {
		batch := missing[start:min(start+fetchBatch, len(missing))]
		g.Go(func() error { return f.fetchBatch(gctx, batch, counter) })
	}
	return g.Wait()
}

func (f *Fetcher) fetchBatch(ctx context.Context, ids []hash.ID, counter *atomic.Int64) error {
	req := &xylemv1.GetObjectsRequest{Repo: f.repo, Ids: make([][]byte, len(ids))}
	for i := range ids {
		req.Ids[i] = ids[i][:]
	}
	stream, err := f.conn.Object.GetObjects(ctx, req)
	if err != nil {
		return err
	}
	got := 0
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if len(msg.Missing) > 0 {
			return fmt.Errorf("server is missing %d of the requested objects (first %x)", len(msg.Missing), msg.Missing[0])
		}
		for _, o := range msg.Objects {
			var id hash.ID
			if len(o.Id) != hash.Size {
				return errors.New("server sent a malformed object id")
			}
			copy(id[:], o.Id)
			if err := f.cache.Put(id, o.Data); err != nil { // verifies the hash
				return err
			}
			got++
			counter.Add(1)
			f.bytes.Add(int64(len(o.Data)))
			f.report()
		}
	}
	if got != len(ids) {
		return fmt.Errorf("asked for %d objects, received %d", len(ids), got)
	}
	return nil
}

func (f *Fetcher) report() {
	if f.progress == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.Stats()
	_, _ = fmt.Fprintf(f.progress, "\rfetching: %d trees, %d blobs, %d bytes ", s.Trees, s.Blobs, s.Bytes)
}

func (f *Fetcher) done() {
	if f.progress != nil {
		_, _ = fmt.Fprintln(f.progress)
	}
}

// CacheTrees reads trees from the cache, fetching missing ones; it
// implements treeops.TreeReader.
type CacheTrees struct {
	F *Fetcher
}

// ReadTree implements treeops.TreeReader.
func (c CacheTrees) ReadTree(ctx context.Context, id hash.ID) (*object.Tree, error) {
	if !c.F.cache.Has(id) {
		var n atomic.Int64
		if err := c.F.ensure(ctx, []hash.ID{id}, &n); err != nil {
			return nil, err
		}
		c.F.trees.Add(n.Load())
	}
	return c.F.readTree(id)
}
