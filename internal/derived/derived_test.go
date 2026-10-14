package derived

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
)

// memSource is an in-memory Source that counts row reads.
type memSource struct {
	core     map[hash.ID]meta.CoreRow
	parents  map[hash.ID][]hash.ID
	coreRead atomic.Int64
}

func newMemSource() *memSource {
	return &memSource{core: map[hash.ID]meta.CoreRow{}, parents: map[hash.ID][]hash.ID{}}
}

func (m *memSource) Core(_ context.Context, ids []hash.ID) (map[hash.ID]meta.CoreRow, error) {
	m.coreRead.Add(int64(len(ids)))
	out := map[hash.ID]meta.CoreRow{}
	for _, id := range ids {
		if r, ok := m.core[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

func (m *memSource) Parents(_ context.Context, ids []hash.ID) (map[hash.ID][]hash.ID, error) {
	out := map[hash.ID][]hash.ID{}
	for _, id := range ids {
		if p, ok := m.parents[id]; ok {
			out[id] = p
		}
	}
	return out, nil
}

// rng is a tiny deterministic generator over ChaCha8 (no math/rand globals).
type rng struct{ src *rand.ChaCha8 }

func newRNG(seed byte) *rng { return &rng{rand.NewChaCha8([32]byte{seed})} }

func (r *rng) intn(n int) int { return int(r.src.Uint64()>>34) % n } // 30 random bits; n is small

// dag is a generated history: nodes in topological order.
type dag struct {
	ids     []hash.ID
	parents map[hash.ID][]hash.ID
	src     *memSource
}

func nodeID(i int) hash.ID {
	return hash.Sum(nil, fmt.Appendf(nil, "node %d", i))
}

// build inserts nodes in order, computing core rows with ComputeCore.
func build(t testing.TB, parentsOf func(i int) []int, n int) *dag {
	t.Helper()
	d := &dag{parents: map[hash.ID][]hash.ID{}, src: newMemSource()}
	for i := range n {
		id := nodeID(i)
		var ps []hash.ID
		for _, p := range parentsOf(i) {
			ps = append(ps, d.ids[p])
		}
		row, err := ComputeCore(context.Background(), d.src, id, ps, hash.ID{0xee})
		if err != nil {
			t.Fatalf("ComputeCore(%d): %v", i, err)
		}
		d.src.core[id] = row
		d.src.parents[id] = ps
		d.parents[id] = ps
		d.ids = append(d.ids, id)
	}
	return d
}

// randomDAG has a few roots, mostly linear first-parent chains with branching,
// and merges with up to three parents.
func randomDAG(t testing.TB, r *rng, n int) *dag {
	return build(t, func(i int) []int {
		if i == 0 || r.intn(40) == 0 {
			return nil // root
		}
		first := i - 1
		if r.intn(3) == 0 {
			first = r.intn(i)
		}
		ps := []int{first}
		for k := r.intn(8); k >= 6 && len(ps) < 3; k-- { // ~25% merges
			if p := r.intn(i); !slices.Contains(ps, p) {
				ps = append(ps, p)
			}
		}
		return ps
	}, n)
}

func (d *dag) ancestors(id hash.ID) map[hash.ID]bool {
	seen := map[hash.ID]bool{id: true}
	stack := []hash.ID{id}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range d.parents[n] {
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return seen
}

func (d *dag) firstParentChain(id hash.ID) []hash.ID { // id, fp(id), …, root
	chain := []hash.ID{id}
	for ps := d.parents[id]; len(ps) > 0; ps = d.parents[ps[0]] {
		chain = append(chain, ps[0])
	}
	return chain
}

func TestComputeCoreMatchesNaive(t *testing.T) {
	t.Parallel()
	d := randomDAG(t, newRNG(1), 400)
	gen := map[hash.ID]int64{}
	for _, id := range d.ids {
		g := int64(1)
		for _, p := range d.parents[id] {
			g = max(g, gen[p]+1)
		}
		gen[id] = g
		row := d.src.core[id]
		chain := d.firstParentChain(id)
		if row.Gen != g || row.FPDepth != int64(len(chain)-1) {
			t.Fatalf("%s: gen %d depth %d, want %d %d", id.Short(6), row.Gen, row.FPDepth, g, len(chain)-1)
		}
		if len(chain) == 1 {
			if !row.FPParent.IsZero() || row.FPJump != id {
				t.Fatalf("root %s: fp_parent %s jump %s", id.Short(6), row.FPParent.Short(6), row.FPJump.Short(6))
			}
		} else if row.FPParent != chain[1] || !slices.Contains(chain[1:], row.FPJump) {
			t.Fatalf("%s: fp_parent or jump not on the first-parent chain", id.Short(6))
		}
	}
}

func TestComputeCoreMissingParent(t *testing.T) {
	t.Parallel()
	src := newMemSource()
	if _, err := ComputeCore(context.Background(), src, nodeID(1), []hash.ID{nodeID(0)}, hash.ID{}); !errors.Is(err, ErrMissing) {
		t.Errorf("missing parent: %v, want ErrMissing", err)
	}
}

func TestAncestorAtDepthDeep(t *testing.T) {
	t.Parallel()
	r := newRNG(2)
	const n = 10_000
	// A deep first-parent tree: mostly linear, sometimes branching off an earlier node.
	d := build(t, func(i int) []int {
		switch {
		case i == 0:
			return nil
		case r.intn(10) == 0:
			return []int{r.intn(i)}
		default:
			return []int{i - 1}
		}
	}, n)
	ctx := context.Background()
	for range 2000 {
		v := d.ids[r.intn(n)]
		chain := d.firstParentChain(v) // chain[k] has depth len-1-k
		depth := len(chain) - 1
		target := r.intn(depth + 1)
		before := d.src.coreRead.Load()
		got, err := AncestorAtDepth(ctx, d.src, v, int64(target))
		if err != nil {
			t.Fatal(err)
		}
		if want := chain[depth-target]; got != want {
			t.Fatalf("AncestorAtDepth(%s, %d) = %s, want %s", v.Short(6), target, got.Short(6), want.Short(6))
		}
		// O(log distance): a few row reads per bit of the distance.
		if reads, limit := d.src.coreRead.Load()-before, int64(6*bits.Len(uint(depth-target))+4); reads > limit {
			t.Errorf("AncestorAtDepth over distance %d read %d rows, limit %d", depth-target, reads, limit)
		}
	}
	if _, err := AncestorAtDepth(ctx, d.src, d.ids[5], 6); err == nil {
		t.Error("depth beyond v accepted")
	}
	if _, err := AncestorAtDepth(ctx, d.src, d.ids[5], -1); err == nil {
		t.Error("negative depth accepted")
	}
}

func TestAncestryMatchesReachability(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for seed := range byte(6) {
		r := newRNG(10 + seed)
		d := randomDAG(t, r, 120)
		anc := map[hash.ID]map[hash.ID]bool{}
		for _, id := range d.ids {
			anc[id] = d.ancestors(id)
		}
		for _, a := range d.ids {
			for _, b := range d.ids {
				got, err := IsAncestor(ctx, d.src, a, b)
				if err != nil {
					t.Fatal(err)
				}
				if got != anc[b][a] {
					t.Fatalf("IsAncestor(%s, %s) = %v, want %v", a.Short(6), b.Short(6), got, anc[b][a])
				}
				fp, err := IsFirstParentAncestor(ctx, d.src, a, b)
				if err != nil {
					t.Fatal(err)
				}
				if want := slices.Contains(d.firstParentChain(b), a); fp != want {
					t.Fatalf("IsFirstParentAncestor(%s, %s) = %v, want %v", a.Short(6), b.Short(6), fp, want)
				}
			}
		}
	}
}

// bruteMergeBases returns the maximal common ancestors of a and b.
func bruteMergeBases(d *dag, a, b hash.ID) []hash.ID {
	ancA, ancB := d.ancestors(a), d.ancestors(b)
	var common []hash.ID
	for _, id := range d.ids {
		if ancA[id] && ancB[id] {
			common = append(common, id)
		}
	}
	var best []hash.ID
	for _, c := range common {
		dominated := false
		for _, o := range common {
			if o != c && d.ancestors(o)[c] {
				dominated = true
				break
			}
		}
		if !dominated {
			best = append(best, c)
		}
	}
	slices.SortFunc(best, hash.Compare)
	return best
}

func TestMergeBasesMatchesBruteForce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	multi := 0
	for seed := range byte(8) {
		r := newRNG(30 + seed)
		d := randomDAG(t, r, 300)
		for range 150 {
			a, b := d.ids[r.intn(len(d.ids))], d.ids[r.intn(len(d.ids))]
			got, err := MergeBases(ctx, d.src, a, b)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.IsSortedFunc(got, func(x, y hash.ID) int {
				gx, gy := d.src.core[x].Gen, d.src.core[y].Gen
				if gx != gy {
					return int(gy - gx)
				}
				return hash.Compare(x, y)
			}) {
				t.Fatalf("MergeBases not sorted by gen desc, id")
			}
			want := bruteMergeBases(d, a, b)
			sorted := slices.Clone(got)
			slices.SortFunc(sorted, hash.Compare)
			if !slices.Equal(sorted, want) {
				t.Fatalf("MergeBases(%s, %s) = %v, want %v", a.Short(6), b.Short(6), sorted, want)
			}
			if len(want) > 1 {
				multi++
			}
		}
	}
	if multi == 0 {
		t.Error("generator never produced multiple merge bases (criss-cross)")
	}
	t.Logf("%d pairs had more than one merge base", multi)
}

func TestMergeBasesDisjointAndSame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := build(t, func(i int) []int {
		switch i {
		case 0, 1:
			return nil // two unrelated roots
		default:
			return []int{i - 2}
		}
	}, 6)
	if got, err := MergeBases(ctx, d.src, d.ids[4], d.ids[5]); err != nil || len(got) != 0 {
		t.Errorf("unrelated histories: %v, %v; want none", got, err)
	}
	if got, err := MergeBases(ctx, d.src, d.ids[4], d.ids[4]); err != nil || !slices.Equal(got, d.ids[4:5]) {
		t.Errorf("same commit: %v, %v", got, err)
	}
	if got, err := MergeBases(ctx, d.src, d.ids[2], d.ids[4]); err != nil || !slices.Equal(got, d.ids[2:3]) {
		t.Errorf("ancestor and descendant: %v, %v", got, err)
	}
	if _, err := MergeBases(ctx, d.src, d.ids[0], hash.ID{9}); !errors.Is(err, ErrMissing) {
		t.Errorf("missing: %v", err)
	}
}

// fakeMeta counts batch loads for CachedSource.
type fakeMeta struct {
	meta.Store
	core    map[hash.ID]meta.CoreRow
	parents map[hash.ID][]hash.ID
	loads   int
}

func (f *fakeMeta) GetCore(_ context.Context, _ meta.RepoID, ids []hash.ID) (map[hash.ID]meta.CoreRow, error) {
	f.loads++
	out := map[hash.ID]meta.CoreRow{}
	for _, id := range ids {
		if r, ok := f.core[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

func (f *fakeMeta) GetParents(_ context.Context, _ meta.RepoID, ids []hash.ID) (map[hash.ID][]hash.ID, error) {
	f.loads++
	out := map[hash.ID][]hash.ID{}
	for _, id := range ids {
		if p, ok := f.parents[id]; ok {
			out[id] = p
		}
	}
	return out, nil
}

func TestCachedSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, b, missing := nodeID(1), nodeID(2), nodeID(3)
	f := &fakeMeta{
		core:    map[hash.ID]meta.CoreRow{a: {ChangesetID: a, Gen: 1}, b: {ChangesetID: b, Gen: 2}},
		parents: map[hash.ID][]hash.ID{a: {}, b: {a}},
	}
	s := NewCachedSource(f, 1, 100)
	if got, _ := s.Core(ctx, []hash.ID{a, b, missing}); len(got) != 2 || f.loads != 1 {
		t.Fatalf("first Core: %d rows, %d loads", len(got), f.loads)
	}
	if got, _ := s.Core(ctx, []hash.ID{a, b}); len(got) != 2 || f.loads != 1 {
		t.Errorf("cached Core reloaded: %d loads", f.loads)
	}
	if _, _ = s.Core(ctx, []hash.ID{missing}); f.loads != 2 {
		t.Errorf("missing id should be re-queried, loads = %d", f.loads)
	}
	if got, _ := s.Parents(ctx, []hash.ID{b, b}); len(got) != 1 || got[b][0] != a || f.loads != 3 {
		t.Errorf("Parents = %v, loads %d", got, f.loads)
	}
	c := nodeID(4)
	s.AddCore(meta.CoreRow{ChangesetID: c, Gen: 3})
	s.AddParents(c, []hash.ID{b})
	if got, _ := s.Core(ctx, []hash.ID{c}); got[c].Gen != 3 || f.loads != 3 {
		t.Errorf("AddCore not served from cache")
	}
	if got, _ := s.Parents(ctx, []hash.ID{c}); len(got[c]) != 1 || f.loads != 3 {
		t.Errorf("AddParents not served from cache")
	}
}

// With generation-ordered paint-down a redundant candidate cannot arise (an
// ancestor of a candidate is always stale before it is popped), so test the
// spec's step 4 directly.
func TestRemoveRedundant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := build(t, func(i int) []int {
		switch i {
		case 0:
			return nil
		case 3:
			return []int{1, 2}
		default:
			return []int{0}
		}
	}, 4) // 0 ← 1, 0 ← 2, {1,2} ← 3
	gens := map[hash.ID]int64{}
	for _, id := range d.ids {
		gens[id] = d.src.core[id].Gen
	}
	got, err := removeRedundant(ctx, d.src, []hash.ID{d.ids[0], d.ids[1], d.ids[2]}, gens)
	if err != nil {
		t.Fatal(err)
	}
	want := []hash.ID{d.ids[1], d.ids[2]}
	slices.SortFunc(want, hash.Compare) // equal gens: sorted by id
	if !slices.Equal(got, want) {
		t.Errorf("removeRedundant = %v, want %v (root 0 is an ancestor of both)", got, want)
	}
}
