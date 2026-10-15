// Package diff implements Myers' O(ND) line diff and unified hunks
// (04-derived-data.md §Kind 5).
//
// Algorithm choice (M4.6): the linear-space variant. Each region is split at
// the midpoint of an optimal edit path found by searching forward and
// backward at once (Myers 1986, §4b), so memory is O(N) and edit scripts are
// minimal. As a safety valve, a region that needs more than MaxCost edits is
// replaced wholesale (all of a's lines deleted, all of b's inserted); the
// result is still a correct diff, just not minimal there.
//
// Among equally minimal scripts, runs of changed lines are then compacted to
// the positions git's diff chooses (compact.go), so blame attributes
// ambiguous lines (blank lines, repeated lines) as git does.
//
// Lines end after each '\n'; a final line without one is still a line. Lines
// are compared as byte strings including their terminator, as git does.
package diff

import "bytes"

// MaxCost bounds the search per region; see the package comment.
const MaxCost = 1 << 14

// binaryProbe is how much of a file IsBinary inspects (as git does).
const binaryProbe = 8 << 10

// IsBinary reports whether content has a NUL byte in its first 8 KiB.
func IsBinary(content []byte) bool {
	return bytes.IndexByte(content[:min(len(content), binaryProbe)], 0) >= 0
}

// Lines splits content after each '\n'. The lines alias content.
func Lines(content []byte) [][]byte {
	var lines [][]byte
	for len(content) > 0 {
		i := bytes.IndexByte(content, '\n')
		if i < 0 {
			return append(lines, content)
		}
		lines = append(lines, content[:i+1])
		content = content[i+1:]
	}
	return lines
}

// Op is an edit operation.
type Op byte

// Edit operations.
const (
	Equal  Op = '='
	Delete Op = '-'
	Insert Op = '+'
)

// Edit is a run of one operation: a[OldStart:OldEnd] and b[NewStart:NewEnd]
// (0-based, half-open). Equal runs have equal lengths; Delete runs have an
// empty new range, Insert runs an empty old range.
type Edit struct {
	Op               Op
	OldStart, OldEnd int
	NewStart, NewEnd int
}

// Diff returns the edit script turning a into b, as alternating runs that
// cover both sequences in order.
func Diff(a, b [][]byte) []Edit {
	ia, ib := intern(a, b)
	var matches []pair
	lcs(ia, ib, 0, 0, MaxCost, &matches)
	matches = compact(ia, ib, matches)
	return toEdits(matches, len(a), len(b))
}

// intern maps lines to small integers so comparisons are cheap.
func intern(a, b [][]byte) ([]int, []int) {
	ids := map[string]int{}
	conv := func(lines [][]byte) []int {
		out := make([]int, len(lines))
		for i, l := range lines {
			id, ok := ids[string(l)]
			if !ok {
				id = len(ids)
				ids[string(l)] = id
			}
			out[i] = id
		}
		return out
	}
	return conv(a), conv(b)
}

// pair is one matched line: a[x] == b[y].
type pair struct{ x, y int }

// lcs appends, in order, the matched lines of a minimal edit script between
// a and b (offset by ax, by in the caller's coordinates). Regions that need
// more than maxCost edits are replaced wholesale.
func lcs(a, b []int, ax, by, maxCost int, out *[]pair) {
	// Common prefix.
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		*out = append(*out, pair{ax + p, by + p})
		p++
	}
	a, b, ax, by = a[p:], b[p:], ax+p, by+p
	// Common suffix (emitted after the middle).
	s := 0
	for s < len(a) && s < len(b) && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	mid := func() {
		a, b := a[:len(a)-s], b[:len(b)-s]
		if len(a) == 0 || len(b) == 0 || disjoint(a, b) {
			return
		}
		x, y, ok := bisect(a, b, maxCost)
		if !ok {
			return // over maxCost: replace the region
		}
		lcs(a[:x], b[:y], ax, by, maxCost, out)
		lcs(a[x:], b[y:], ax+x, by+y, maxCost, out)
	}
	mid()
	for i := s; i > 0; i-- {
		*out = append(*out, pair{ax + len(a) - i, by + len(b) - i})
	}
}

// bisect finds a point (x, y) on an optimal edit path through the middle of
// the edit graph by running the forward and backward searches until they
// overlap. ok is false when a and b have nothing in common or the search
// exceeds MaxCost.
func bisect(a, b []int, maxCost int) (x, y int, ok bool) {
	n, m := len(a), len(b)
	maxD := (n + m + 1) / 2
	off := maxD
	size := 2*maxD + 2
	vf := make([]int, size)
	vb := make([]int, size)
	for i := range vf {
		vf[i], vb[i] = -1, -1
	}
	vf[off+1], vb[off+1] = 0, 0
	delta := n - m
	front := delta%2 != 0
	k1start, k1end, k2start, k2end := 0, 0, 0, 0
	for d := 0; d < maxD; d++ {
		if d > maxCost {
			return 0, 0, false
		}
		for k1 := -d + k1start; k1 <= d-k1end; k1 += 2 {
			i := off + k1
			var x1 int
			if k1 == -d || (k1 != d && vf[i-1] < vf[i+1]) {
				x1 = vf[i+1]
			} else {
				x1 = vf[i-1] + 1
			}
			y1 := x1 - k1
			for x1 < n && y1 < m && a[x1] == b[y1] {
				x1++
				y1++
			}
			vf[i] = x1
			switch {
			case x1 > n:
				k1end += 2
			case y1 > m:
				k1start += 2
			case front:
				j := off + delta - k1
				if j >= 0 && j < size && vb[j] != -1 && x1 >= n-vb[j] {
					return x1, y1, true
				}
			}
		}
		for k2 := -d + k2start; k2 <= d-k2end; k2 += 2 {
			i := off + k2
			var x2 int
			if k2 == -d || (k2 != d && vb[i-1] < vb[i+1]) {
				x2 = vb[i+1]
			} else {
				x2 = vb[i-1] + 1
			}
			y2 := x2 - k2
			for x2 < n && y2 < m && a[n-x2-1] == b[m-y2-1] {
				x2++
				y2++
			}
			vb[i] = x2
			switch {
			case x2 > n:
				k2end += 2
			case y2 > m:
				k2start += 2
			case !front:
				j := off + delta - k2
				if j >= 0 && j < size && vf[j] != -1 {
					x1 := vf[j]
					y1 := off + x1 - j
					if x1 >= n-x2 {
						return x1, y1, true
					}
				}
			}
		}
	}
	return 0, 0, false
}

// disjoint reports whether a and b share no line, in O(len(a)+len(b)).
func disjoint(a, b []int) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	seen := make(map[int]struct{}, len(a))
	for _, v := range a {
		seen[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := seen[v]; ok {
			return false
		}
	}
	return true
}

// toEdits converts ordered matches into alternating runs.
func toEdits(matches []pair, n, m int) []Edit {
	var edits []Edit
	add := func(op Op, os, oe, ns, ne int) {
		if oe == os && ne == ns {
			return
		}
		if k := len(edits) - 1; k >= 0 && edits[k].Op == op {
			edits[k].OldEnd, edits[k].NewEnd = oe, ne
			return
		}
		edits = append(edits, Edit{Op: op, OldStart: os, OldEnd: oe, NewStart: ns, NewEnd: ne})
	}
	x, y := 0, 0
	for _, p := range matches {
		add(Delete, x, p.x, y, y)
		add(Insert, p.x, p.x, y, p.y)
		add(Equal, p.x, p.x+1, p.y, p.y+1)
		x, y = p.x+1, p.y+1
	}
	add(Delete, x, n, y, y)
	add(Insert, n, n, y, m)
	return edits
}
