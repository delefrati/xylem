// Package gc finds unreferenced objects (M9.4). It marks every object
// reachable from the changeset root trees and derived value_refs of every
// repository (objects are shared across repositories), then lists the stored
// objects that are not marked and are older than a grace period. Only the
// dry run exists: deleting them (the sweep) waits for ADR-023's approval.
package gc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/store"
)

// DefaultMinAge is the grace period: younger unreferenced objects may belong
// to a write in progress (a blob uploaded before its CreateChangeset, the
// trees of a changeset not inserted yet), so they are never orphans.
const DefaultMinAge = 7 * 24 * time.Hour

// Options configure DryRun.
type Options struct {
	MinAge  time.Duration    // grace period; xylemd gc defaults to DefaultMinAge
	Workers int              // concurrent tree reads (default 16)
	Now     func() time.Time // default time.Now
}

// Orphan is an unreferenced object older than the grace period.
type Orphan struct {
	store.ObjectInfo
	Kind object.Kind // 0 if the header could not be read
}

// Count is a number of objects and their stored bytes.
type Count struct {
	Objects int
	Bytes   int64
}

func (c *Count) add(size int64) { c.Objects++; c.Bytes += size }

// Report summarizes a dry run.
type Report struct {
	Repos     int
	Roots     int   // distinct root trees and derived value_refs
	Reachable int   // distinct objects reachable from the roots
	Stored    Count // everything the store listed
	Orphans   Count // unreferenced and older than MinAge
	ByKind    map[object.Kind]*Count
	Young     Count // unreferenced but within the grace period
	// Missing counts reachable objects the store did not list: a store
	// problem `xylemd verify --objects` would also report.
	Missing int
}

// ErrNotListable reports a store that cannot enumerate its objects.
var ErrNotListable = errors.New("the object store cannot list its objects")

// DryRun marks, lists and reports; orphan (optional) is called for each
// orphan as it is found.
func DryRun(ctx context.Context, m meta.Store, objs store.ObjectStore, opts Options, orphan func(Orphan) error) (*Report, error) {
	lister, ok := objs.(store.Lister)
	if !ok {
		return nil, ErrNotListable
	}
	opts.MinAge = max(opts.MinAge, 0)
	if opts.Workers <= 0 {
		opts.Workers = 16
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	// Start the clock before marking: an object written during the mark
	// (and referenced by a row the mark missed) is younger than the cutoff.
	cutoff := opts.Now().Add(-opts.MinAge)

	rep := &Report{ByKind: map[object.Kind]*Count{}}
	reach, err := mark(ctx, m, objs, opts.Workers, rep)
	if err != nil {
		return nil, err
	}
	rep.Reachable = len(reach)

	listed := 0
	err = lister.List(ctx, func(info store.ObjectInfo) error {
		rep.Stored.add(info.Size)
		if _, ok := reach[info.ID]; ok {
			listed++
			return nil
		}
		if info.ModTime.After(cutoff) {
			rep.Young.add(info.Size)
			return nil
		}
		o := Orphan{ObjectInfo: info}
		if stored, err := objs.Get(ctx, info.ID); err == nil {
			if kind, _, err := object.PeekHeader(stored); err == nil {
				o.Kind = kind
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		rep.Orphans.add(info.Size)
		if rep.ByKind[o.Kind] == nil {
			rep.ByKind[o.Kind] = &Count{}
		}
		rep.ByKind[o.Kind].add(info.Size)
		if orphan != nil {
			return orphan(o)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	rep.Missing = rep.Reachable - listed
	return rep, nil
}

// mark returns every object reachable from every repository's roots,
// reading trees level by level with up to workers reads at once.
func mark(ctx context.Context, m meta.Store, objs store.ObjectStore, workers int, rep *Report) (map[hash.ID]struct{}, error) {
	repos, err := m.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	rep.Repos = len(repos)
	reach := map[hash.ID]struct{}{}
	var frontier []hash.ID // trees to read
	for id := range repos {
		err := m.ForEachObjectRoot(ctx, id, func(obj hash.ID, derived bool) error {
			if _, ok := reach[obj]; ok {
				return nil
			}
			reach[obj] = struct{}{}
			rep.Roots++
			if !derived {
				frontier = append(frontier, obj)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var mu sync.Mutex
	for len(frontier) > 0 {
		var next []hash.ID
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(workers)
		for _, id := range frontier {
			g.Go(func() error {
				stored, err := objs.Get(gctx, id)
				if errors.Is(err, store.ErrNotFound) {
					return nil // counted as missing after the listing
				}
				if err != nil {
					return err
				}
				kind, body, err := object.DecodeVerify(id, stored)
				if err != nil {
					return fmt.Errorf("tree %s: %w", id, err)
				}
				if kind != object.KindTree {
					return fmt.Errorf("root %s is a %s, not a tree", id, kind)
				}
				tree, err := object.DecodeTree(body)
				if err != nil {
					return fmt.Errorf("tree %s: %w", id, err)
				}
				mu.Lock()
				defer mu.Unlock()
				for _, e := range tree.Entries {
					if _, ok := reach[e.ID]; ok {
						continue
					}
					reach[e.ID] = struct{}{}
					if e.Mode == object.ModeTree {
						next = append(next, e.ID)
					}
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return nil, fmt.Errorf("marking: %w", err)
		}
		frontier = next
	}
	return reach, nil
}
