package derived

import (
	"container/heap"
	"context"
	"fmt"
	"slices"

	"github.com/delefrati/xylem/internal/hash"
)

// AncestorAtDepth returns the first-parent ancestor of v at first-parent
// depth d, following jump pointers: O(log(depth(v) − d)) row reads.
func AncestorAtDepth(ctx context.Context, src Source, v hash.ID, d int64) (hash.ID, error) {
	rows, err := core(ctx, src, v)
	if err != nil {
		return hash.ID{}, err
	}
	cur := rows[v]
	if d < 0 || d > cur.FPDepth {
		return hash.ID{}, fmt.Errorf("ancestor of %s at depth %d: depth must be in [0, %d]", v.Short(12), d, cur.FPDepth)
	}
	for cur.FPDepth > d {
		next := cur.FPParent
		if jrows, err := core(ctx, src, cur.FPJump); err != nil {
			return hash.ID{}, err
		} else if jrows[cur.FPJump].FPDepth >= d {
			next = cur.FPJump
		}
		nrows, err := core(ctx, src, next)
		if err != nil {
			return hash.ID{}, err
		}
		cur = nrows[next]
	}
	return cur.ChangesetID, nil
}

// IsFirstParentAncestor reports whether a is on b's first-parent chain
// (including b itself).
func IsFirstParentAncestor(ctx context.Context, src Source, a, b hash.ID) (bool, error) {
	rows, err := core(ctx, src, a, b)
	if err != nil {
		return false, err
	}
	if rows[a].FPDepth > rows[b].FPDepth {
		return false, nil
	}
	at, err := AncestorAtDepth(ctx, src, b, rows[a].FPDepth)
	return at == a, err
}

// IsAncestor reports whether a is an ancestor of b (or equal to it) in the full
// DAG: generation-number check, then the first-parent fast path, then a BFS
// over all parents that prunes nodes with gen < gen(a).
func IsAncestor(ctx context.Context, src Source, a, b hash.ID) (bool, error) {
	if a == b {
		return true, nil
	}
	rows, err := core(ctx, src, a, b)
	if err != nil {
		return false, err
	}
	genA := rows[a].Gen
	if genA >= rows[b].Gen {
		return false, nil
	}
	if ok, err := IsFirstParentAncestor(ctx, src, a, b); err != nil || ok {
		return ok, err
	}
	seen := map[hash.ID]bool{b: true}
	frontier := []hash.ID{b}
	for len(frontier) > 0 {
		parents, err := src.Parents(ctx, frontier)
		if err != nil {
			return false, err
		}
		var next []hash.ID
		for _, n := range frontier {
			ps, ok := parents[n]
			if !ok {
				return false, fmt.Errorf("parents of %s: %w", n.Short(12), ErrMissing)
			}
			for _, p := range ps {
				if p == a {
					return true, nil
				}
				if !seen[p] {
					seen[p] = true
					next = append(next, p)
				}
			}
		}
		if len(next) == 0 {
			break
		}
		prows, err := core(ctx, src, next...)
		if err != nil {
			return false, err
		}
		frontier = next[:0:0]
		for _, p := range next {
			if prows[p].Gen > genA { // gen == genA and p != a cannot reach a
				frontier = append(frontier, p)
			}
		}
	}
	return false, nil
}

// paint-down flags
const (
	flagA uint8 = 1 << iota
	flagB
	flagStale
)

type queued struct {
	id  hash.ID
	gen int64
}

// genQueue is a max-heap by gen, ties by id ascending, so pops are deterministic.
type genQueue []queued

func (q genQueue) Len() int { return len(q) }
func (q genQueue) Less(i, j int) bool {
	if q[i].gen != q[j].gen {
		return q[i].gen > q[j].gen
	}
	return hash.Compare(q[i].id, q[j].id) < 0
}
func (q genQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *genQueue) Push(x any)   { *q = append(*q, x.(queued)) }
func (q *genQueue) Pop() any {
	old := *q
	x := old[len(old)-1]
	*q = old[:len(old)-1]
	return x
}

// MergeBases returns the best common ancestors of a and b, as
// `git merge-base --all` does: git's paint-down with generation numbers, then
// candidates that are ancestors of other candidates are removed. The result is
// sorted by gen descending, then id.
func MergeBases(ctx context.Context, src Source, a, b hash.ID) ([]hash.ID, error) {
	rows, err := core(ctx, src, a, b)
	if err != nil {
		return nil, err
	}
	if a == b {
		return []hash.ID{a}, nil
	}
	gens := map[hash.ID]int64{a: rows[a].Gen, b: rows[b].Gen}
	flags := map[hash.ID]uint8{a: flagA, b: flagB}
	q := &genQueue{{a, gens[a]}, {b, gens[b]}}
	heap.Init(q)
	var candidates []hash.ID
	isCandidate := map[hash.ID]bool{}

	for hasNonStale(*q, flags) {
		n := heap.Pop(q).(queued)
		f := flags[n.id]
		if f&(flagA|flagB) == flagA|flagB {
			if !isCandidate[n.id] && f&flagStale == 0 {
				isCandidate[n.id] = true
				candidates = append(candidates, n.id)
			}
			f |= flagStale
		}
		parents, err := src.Parents(ctx, []hash.ID{n.id})
		if err != nil {
			return nil, err
		}
		ps, ok := parents[n.id]
		if !ok {
			return nil, fmt.Errorf("parents of %s: %w", n.id.Short(12), ErrMissing)
		}
		var toLoad []hash.ID
		for _, p := range ps {
			if _, known := gens[p]; !known {
				toLoad = append(toLoad, p)
			}
		}
		if len(toLoad) > 0 {
			prows, err := core(ctx, src, toLoad...)
			if err != nil {
				return nil, err
			}
			for _, p := range toLoad {
				gens[p] = prows[p].Gen
			}
		}
		for _, p := range ps {
			if flags[p]&f == f {
				continue
			}
			flags[p] |= f
			heap.Push(q, queued{p, gens[p]})
		}
	}
	return removeRedundant(ctx, src, candidates, gens)
}

// hasNonStale reports whether any queued node is not yet stale; the walk stops
// when every remaining node is stale (git's queue_has_nonstale).
func hasNonStale(q genQueue, flags map[hash.ID]uint8) bool {
	for _, n := range q {
		if flags[n.id]&flagStale == 0 {
			return true
		}
	}
	return false
}

// removeRedundant drops candidates that are ancestors of other candidates and
// sorts the rest by gen descending, then id. With generation-ordered
// paint-down this never drops anything (git needs it because it orders by
// commit date); it is kept as the spec's step 4 and a cheap safeguard.
func removeRedundant(ctx context.Context, src Source, candidates []hash.ID, gens map[hash.ID]int64) ([]hash.ID, error) {
	var out []hash.ID
	for i, c := range candidates {
		redundant := false
		for j, other := range candidates {
			if i == j {
				continue
			}
			ok, err := IsAncestor(ctx, src, c, other)
			if err != nil {
				return nil, err
			}
			if ok {
				redundant = true
				break
			}
		}
		if !redundant {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(x, y hash.ID) int {
		if gens[x] != gens[y] {
			if gens[x] > gens[y] {
				return -1
			}
			return 1
		}
		return hash.Compare(x, y)
	})
	return out, nil
}
