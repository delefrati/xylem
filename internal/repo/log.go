package repo

import (
	"context"
	"fmt"
	"math"

	"github.com/delefrati/xylem/internal/derived"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/paths"
)

// MaxLogSteps bounds the changesets one Log page walks one by one (draft
// segments and histories without a mainline ancestor); the page then ends
// early with a cursor (05 §Server implementation notes).
const MaxLogSteps = 100_000

// LogCursor is the position of a first-parent log walk.
type LogCursor struct {
	Tip       hash.ID // the revision the log started from
	Depth     int64   // fp_depth of the next changeset to look at
	Mainline  bool    // path log: walking the mainline index
	MaxDepth  int64   // path log, mainline phase: mainline depth bound
	BeforeSeq int64   // path log, mainline phase: seq bound (exclusive)
}

// StartLog returns the cursor for a log of tip.
func (r *Repo) StartLog(ctx context.Context, tip hash.ID) (LogCursor, error) {
	rows, err := r.source.Core(ctx, []hash.ID{tip})
	if err != nil {
		return LogCursor{}, err
	}
	row, ok := rows[tip]
	if !ok {
		return LogCursor{}, fmt.Errorf("%w: changeset %s", ErrNotFound, tip)
	}
	return LogCursor{Tip: tip, Depth: row.FPDepth}, nil
}

// Log returns up to limit changesets of the first-parent history from
// cur.Tip, newest first, and the cursor of the next page (nil at the end).
// With a path, only changesets whose changes (relative to their first parent)
// touch path or anything below it are returned, as `git log --first-parent
// -- path` does (04 §Kind 3).
func (r *Repo) Log(ctx context.Context, cur LogCursor, path []byte, limit int) ([]hash.ID, *LogCursor, error) {
	if len(path) > 0 {
		if err := paths.ValidPath(path); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	if limit <= 0 {
		return nil, nil, fmt.Errorf("%w: page size must be positive", ErrInvalid)
	}
	if cur.Mainline {
		return r.mainlinePathLog(ctx, cur, path, nil, limit)
	}
	var ids []hash.ID
	depth := cur.Depth
	node, err := r.fpAt(ctx, cur.Tip, depth)
	if err != nil {
		return nil, nil, err
	}
	for steps := 0; depth >= 0 && len(ids) < limit; steps++ {
		onMain, err := r.isMainline(ctx, node, depth)
		if err != nil {
			return nil, nil, err
		}
		if onMain {
			if len(path) > 0 {
				next := cur
				next.Mainline, next.MaxDepth, next.BeforeSeq = true, depth, math.MaxInt64
				return r.mainlinePathLog(ctx, next, path, ids, limit)
			}
			rows, err := r.meta.MainlineRange(ctx, r.id, MainRef, depth, limit-len(ids))
			if err != nil {
				return nil, nil, err
			}
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			depth -= int64(len(rows))
			break
		}
		if steps == MaxLogSteps {
			break // cost limit: return what we have with a cursor
		}
		touched := true
		if len(path) > 0 {
			if touched, err = r.touches(ctx, node, path); err != nil {
				return nil, nil, err
			}
		}
		if touched {
			ids = append(ids, node)
		}
		if depth--; depth < 0 {
			break
		}
		if node, err = r.fpParent(ctx, node); err != nil {
			return nil, nil, err
		}
	}
	if depth < 0 {
		return ids, nil, nil
	}
	next := cur
	next.Depth = depth
	return ids, &next, nil
}

// mainlinePathLog continues a path log on the mainline index.
func (r *Repo) mainlinePathLog(ctx context.Context, cur LogCursor, path []byte, ids []hash.ID, limit int) ([]hash.ID, *LogCursor, error) {
	want := limit - len(ids)
	hits, err := r.meta.PathHistoryMainline(ctx, r.id, MainRef, path, cur.MaxDepth, cur.BeforeSeq, want)
	if err != nil {
		return nil, nil, err
	}
	for _, h := range hits {
		ids = append(ids, h.ChangesetID)
	}
	if len(hits) < want {
		return ids, nil, nil
	}
	next := cur
	next.BeforeSeq = hits[len(hits)-1].Seq
	return ids, &next, nil
}

func (r *Repo) fpAt(ctx context.Context, tip hash.ID, depth int64) (hash.ID, error) {
	if depth < 0 {
		return hash.ID{}, nil
	}
	return derived.AncestorAtDepth(ctx, r.source, tip, depth)
}

func (r *Repo) fpParent(ctx context.Context, id hash.ID) (hash.ID, error) {
	rows, err := r.source.Core(ctx, []hash.ID{id})
	if err != nil {
		return hash.ID{}, err
	}
	return rows[id].FPParent, nil
}

// isMainline reports whether id is main's mainline entry at depth.
func (r *Repo) isMainline(ctx context.Context, id hash.ID, depth int64) (bool, error) {
	depths, err := r.meta.MainlineLookup(ctx, r.id, MainRef, []hash.ID{id})
	if err != nil {
		return false, err
	}
	d, ok := depths[id]
	return ok && d == depth, nil
}

// touches reports whether changeset id changes path or anything below it.
func (r *Repo) touches(ctx context.Context, id hash.ID, path []byte) (bool, error) {
	cs, err := r.Changeset(ctx, id)
	if err != nil {
		return false, err
	}
	for _, ch := range cs.Changes {
		if paths.IsUnder(ch.Path, path) {
			return true, nil
		}
	}
	return false, nil
}
