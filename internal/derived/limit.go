package derived

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
)

// ErrTooExpensive reports a query that visited more changesets than allowed.
var ErrTooExpensive = errors.New("query visits too many changesets")

// Limited wraps src so that loading the parents of more than maxNodes
// changesets fails with ErrTooExpensive. Use one per query.
func Limited(src Source, maxNodes int64) Source {
	return &limited{src: src, left: maxNodes}
}

type limited struct {
	src  Source
	left int64
	used atomic.Int64
}

func (l *limited) Core(ctx context.Context, ids []hash.ID) (map[hash.ID]meta.CoreRow, error) {
	return l.src.Core(ctx, ids)
}

func (l *limited) Parents(ctx context.Context, ids []hash.ID) (map[hash.ID][]hash.ID, error) {
	if l.used.Add(int64(len(ids))) > l.left {
		return nil, fmt.Errorf("%w (limit %d)", ErrTooExpensive, l.left)
	}
	return l.src.Parents(ctx, ids)
}
