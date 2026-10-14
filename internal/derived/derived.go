// Package derived computes derived data (04-derived-data.md). This file holds
// kind 1, core: generation numbers, first-parent depth and the skew-binary
// jump pointer, and the ancestry queries built on them. Other kinds (mainline,
// path history, blame) are added in later milestones.
package derived

import (
	"context"
	"errors"
	"fmt"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
)

// ErrMissing reports a changeset whose core row or parents are not stored.
var ErrMissing = errors.New("changeset not found")

// Source reads core rows and parent lists in batches. Missing ids are absent
// from the results. Implementations are safe for concurrent use.
type Source interface {
	Core(ctx context.Context, ids []hash.ID) (map[hash.ID]meta.CoreRow, error)
	Parents(ctx context.Context, ids []hash.ID) (map[hash.ID][]hash.ID, error)
}

// CachedSource implements Source over a meta.Store with LRUs. Rows are
// immutable, so cached values never need invalidation.
type CachedSource struct {
	store   meta.Store
	repo    meta.RepoID
	core    *lru.Cache[hash.ID, meta.CoreRow]
	parents *lru.Cache[hash.ID, []hash.ID]
}

// NewCachedSource caches up to size core rows and size parent lists.
func NewCachedSource(store meta.Store, repo meta.RepoID, size int) *CachedSource {
	core, _ := lru.New[hash.ID, meta.CoreRow](max(size, 1))
	parents, _ := lru.New[hash.ID, []hash.ID](max(size, 1))
	return &CachedSource{store: store, repo: repo, core: core, parents: parents}
}

// AddCore caches a row, e.g. one just inserted.
func (s *CachedSource) AddCore(row meta.CoreRow) { s.core.Add(row.ChangesetID, row) }

// AddParents caches a parent list, e.g. of a changeset just inserted.
func (s *CachedSource) AddParents(id hash.ID, parents []hash.ID) { s.parents.Add(id, parents) }

// Core implements Source.
func (s *CachedSource) Core(ctx context.Context, ids []hash.ID) (map[hash.ID]meta.CoreRow, error) {
	return cachedBatch(ctx, s.core, ids, func(ctx context.Context, miss []hash.ID) (map[hash.ID]meta.CoreRow, error) {
		return s.store.GetCore(ctx, s.repo, miss)
	})
}

// Parents implements Source.
func (s *CachedSource) Parents(ctx context.Context, ids []hash.ID) (map[hash.ID][]hash.ID, error) {
	return cachedBatch(ctx, s.parents, ids, func(ctx context.Context, miss []hash.ID) (map[hash.ID][]hash.ID, error) {
		return s.store.GetParents(ctx, s.repo, miss)
	})
}

func cachedBatch[V any](ctx context.Context, cache *lru.Cache[hash.ID, V], ids []hash.ID,
	load func(context.Context, []hash.ID) (map[hash.ID]V, error),
) (map[hash.ID]V, error) {
	out := make(map[hash.ID]V, len(ids))
	var miss []hash.ID
	for _, id := range ids {
		if v, ok := cache.Get(id); ok {
			out[id] = v
		} else if _, dup := out[id]; !dup {
			miss = append(miss, id)
		}
	}
	if len(miss) == 0 {
		return out, nil
	}
	loaded, err := load(ctx, miss)
	if err != nil {
		return nil, err
	}
	for id, v := range loaded {
		cache.Add(id, v)
		out[id] = v
	}
	return out, nil
}

// core loads the rows of ids, failing with ErrMissing if any is absent.
func core(ctx context.Context, src Source, ids ...hash.ID) (map[hash.ID]meta.CoreRow, error) {
	rows, err := src.Core(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, ok := rows[id]; !ok {
			return nil, fmt.Errorf("core row of %s: %w", id.Short(12), ErrMissing)
		}
	}
	return rows, nil
}

// ComputeCore returns the core row of a new changeset with the given parents
// and root tree (04 §Kind 1):
//
//	gen       = 1 + max(gen(parents)); roots: 1
//	fp_depth  = fp_depth(parents[0]) + 1; roots: 0
//	fp_parent = parents[0]; roots: none
//	fp_jump   = skew-binary jump pointer; roots point to themselves
//
// It loads the parents' rows, then jump(p0) and jump(jump(p0)).
func ComputeCore(ctx context.Context, src Source, id hash.ID, parents []hash.ID, rootTree hash.ID) (meta.CoreRow, error) {
	row := meta.CoreRow{ChangesetID: id, RootTree: rootTree, Gen: 1, FPJump: id}
	if len(parents) == 0 {
		return row, nil
	}
	rows, err := core(ctx, src, parents...)
	if err != nil {
		return meta.CoreRow{}, fmt.Errorf("parents of %s: %w", id.Short(12), err)
	}
	for _, p := range parents {
		row.Gen = max(row.Gen, rows[p].Gen+1)
	}
	p := rows[parents[0]]
	row.FPParent = p.ChangesetID
	row.FPDepth = p.FPDepth + 1

	jrows, err := core(ctx, src, p.FPJump)
	if err != nil {
		return meta.CoreRow{}, err
	}
	j := jrows[p.FPJump]
	jjrows, err := core(ctx, src, j.FPJump)
	if err != nil {
		return meta.CoreRow{}, err
	}
	jj := jjrows[j.FPJump]
	if p.FPDepth-j.FPDepth == j.FPDepth-jj.FPDepth {
		row.FPJump = jj.ChangesetID
	} else {
		row.FPJump = p.ChangesetID
	}
	return row, nil
}
