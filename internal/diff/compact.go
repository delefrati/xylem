package diff

// Compaction (M6.9): among the many minimal edit scripts, git's diff picks
// a canonical one by sliding each run of changed lines. A run can slide
// when the line just past one end equals the line at its other end. Runs
// first slide as far up as they go, then down as far as they go (merging
// with runs they touch). If some position puts the run next to a change in
// the other file (so deletions and insertions form one hunk), the lowest such
// position wins. Otherwise the run stays at the bottom. The first file is
// compacted against the second, then the second against the first. This is
// what blame depends on: which equal lines are matched.

// changed returns per-line change flags for both sides from ordered matches.
func changed(matches []pair, n, m int) (ca, cb []bool) {
	ca, cb = make([]bool, n), make([]bool, m)
	for i := range ca {
		ca[i] = true
	}
	for j := range cb {
		cb[j] = true
	}
	for _, p := range matches {
		ca[p.x], cb[p.y] = false, false
	}
	return ca, cb
}

// matchesOf pairs the unchanged lines of both sides in order.
func matchesOf(ca, cb []bool) []pair {
	var out []pair
	j := 0
	for i, c := range ca {
		if c {
			continue
		}
		for cb[j] {
			j++
		}
		out = append(out, pair{i, j})
		j++
	}
	return out
}

// anchors reports, for each k, whether the other file has changed lines
// between its k-th and (k+1)-th unchanged lines (k = 0: before the first).
func anchors(c []bool) []bool {
	unchanged := 0
	for _, x := range c {
		if !x {
			unchanged++
		}
	}
	out := make([]bool, unchanged+1)
	k := 0
	for _, x := range c {
		if x {
			out[k] = true
		} else {
			k++
		}
	}
	return out
}

// compactSide slides the change runs of x (flags cx) as described above.
// other[k] says whether the other file changes at anchor k.
func compactSide(x []int, cx []bool, other []bool) {
	n := len(x)
	k := 0 // unchanged lines of x before i: the anchor of position i
	for i := 0; i < n; {
		if !cx[i] {
			k++
			i++
			continue
		}
		s, e := i, i
		for e < n && cx[e] {
			e++
		}
		aligned := -1
		for {
			size := e - s
			// Up as far as possible, absorbing runs above.
			for s > 0 && !cx[s-1] && x[s-1] == x[e-1] {
				cx[s-1], cx[e-1] = true, false
				s, e, k = s-1, e-1, k-1
				for s > 0 && cx[s-1] {
					s--
				}
			}
			earliest := e
			aligned = -1
			if other[k] {
				aligned = e
			}
			// Down as far as possible, absorbing runs below.
			for e < n && !cx[e] && x[s] == x[e] {
				cx[s], cx[e] = false, true
				s, e, k = s+1, e+1, k+1
				for e < n && cx[e] {
					e++
				}
				if other[k] {
					aligned = e
				}
			}
			_ = earliest
			if e-s == size {
				break
			}
		}
		// Back up to the lowest position next to a change in the other file.
		for aligned >= 0 && e > aligned {
			cx[s-1], cx[e-1] = true, false
			s, e, k = s-1, e-1, k-1
		}
		i = e
	}
}

// compact canonicalizes ordered matches between a and b.
func compact(a, b []int, matches []pair) []pair {
	ca, cb := changed(matches, len(a), len(b))
	compactSide(a, ca, anchors(cb))
	compactSide(b, cb, anchors(ca))
	return matchesOf(ca, cb)
}
