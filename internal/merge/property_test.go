package merge

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

func intn(r *rand.ChaCha8, n int) int { return int(r.Uint64()>>34) % n }

func join(lines []string) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l)
	}
	return b.Bytes()
}

// edit replaces lines[start:end] with repl.
type edit struct {
	start, end int
	repl       []string
}

func apply(lines []string, edits ...edit) []string {
	out := slices.Clone(lines)
	slices.SortFunc(edits, func(a, b edit) int { return b.start - a.start }) // back to front
	for _, e := range edits {
		out = slices.Concat(out[:e.start], e.repl, out[e.end:])
	}
	return out
}

func randomEdit(r *rand.ChaCha8, lo, hi int, tag string) edit {
	start := lo + intn(r, hi-lo+1)
	end := start + intn(r, min(3, hi-start)+1)
	repl := make([]string, intn(r, 3))
	for i := range repl {
		repl[i] = fmt.Sprintf("%s %d\n", tag, intn(r, 1000))
	}
	if start == end && len(repl) == 0 {
		repl = []string{tag + " insert\n"}
	}
	return edit{start, end, repl}
}

// Non-overlapping edits (an unchanged base line between them) merge cleanly
// and the result has both; the merge is symmetric.
func TestMergeDisjointEdits(t *testing.T) {
	t.Parallel()
	r := rand.NewChaCha8([32]byte{3})
	for i := range 2000 {
		n := 4 + intn(r, 30)
		base := make([]string, n)
		for j := range base {
			base[j] = fmt.Sprintf("line %d\n", j) // unique lines: a single correct alignment
		}
		split := 1 + intn(r, n-2) // base[split] stays unchanged and separates the edits
		eo := randomEdit(r, 0, split-1, "ours")
		et := randomEdit(r, split+1, n, "theirs")
		if i%2 == 1 {
			eo, et = randomEdit(r, split+1, n, "ours"), randomEdit(r, 0, split-1, "theirs")
		}
		if eo.end > split && eo.start <= split || et.end > split && et.start <= split {
			t.Fatal("generator bug")
		}
		b, o, th := join(base), join(apply(base, eo)), join(apply(base, et))
		want := join(apply(base, eo, et))
		got, regions, err := Merge3(b, o, th)
		if err != nil || len(regions) != 0 || !bytes.Equal(got, want) {
			t.Fatalf("Merge3(%q, %q, %q) = %q, %v, %v; want %q", b, o, th, got, regions, err, want)
		}
		swapped, regions, err := Merge3(b, th, o)
		if err != nil || len(regions) != 0 || !bytes.Equal(swapped, got) {
			t.Fatalf("not symmetric: %q vs %q (%v, %v)", swapped, got, regions, err)
		}
	}
}

func randomText(r *rand.ChaCha8) []byte {
	var b bytes.Buffer
	for range intn(r, 12) {
		b.WriteString([]string{"a\n", "b\n", "\n", "c\n", "a"}[intn(r, 5)])
	}
	return b.Bytes()
}

// Merge3(b, b, x) == x and Merge3(b, x, x) == x for any texts, and clean
// merges of arbitrary edits are symmetric.
func TestMergeIdentities(t *testing.T) {
	t.Parallel()
	r := rand.NewChaCha8([32]byte{4})
	for range 5000 {
		b, x, y := randomText(r), randomText(r), randomText(r)
		for _, tc := range [][3][]byte{{b, b, x}, {b, x, b}, {b, x, x}} {
			got, regions, err := Merge3(tc[0], tc[1], tc[2])
			if err != nil || len(regions) != 0 || !bytes.Equal(got, x) {
				t.Fatalf("Merge3(%q, %q, %q) = %q, %v, %v; want %q", tc[0], tc[1], tc[2], got, regions, err, x)
			}
		}
		xy, rxy, err1 := Merge3(b, x, y)
		yx, ryx, err2 := Merge3(b, y, x)
		if err1 != nil || err2 != nil || (len(rxy) == 0) != (len(ryx) == 0) {
			t.Fatalf("Merge3 of %q, %q, %q: clean one way only (%v / %v)", b, x, y, rxy, ryx)
		}
		if len(rxy) == 0 && !bytes.Equal(xy, yx) {
			t.Fatalf("clean merge not symmetric: %q vs %q", xy, yx)
		}
	}
}

func FuzzMerge3(f *testing.F) {
	f.Add([]byte("a\nb\nc\n"), []byte("a\nB\nc\n"), []byte("a\nb\nC\n"))
	f.Add([]byte(""), []byte("x\n"), []byte("y"))
	f.Add([]byte("a\nb"), []byte("a\nb\n"), []byte("b"))
	f.Fuzz(func(t *testing.T, base, ours, theirs []byte) {
		got, regions, err := Merge3(base, ours, theirs)
		if err != nil {
			return // binary
		}
		if len(regions) == 0 && bytes.Equal(ours, theirs) && !bytes.Equal(got, ours) {
			t.Fatalf("identical sides merged to %q", got)
		}
		if same, _, _ := Merge3(base, base, theirs); !bytes.Equal(same, theirs) {
			t.Fatalf("Merge3(b, b, x) = %q, want %q", same, theirs)
		}
		for _, rg := range regions {
			if rg.BaseStart > rg.BaseEnd || rg.OursStart > rg.OursEnd || rg.TheirsStart > rg.TheirsEnd {
				t.Fatalf("bad region %+v", rg)
			}
		}
	})
}
