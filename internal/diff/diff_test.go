package diff

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestLines(t *testing.T) {
	t.Parallel()
	tests := map[string][]string{
		"":           nil,
		"a":          {"a"},
		"a\n":        {"a\n"},
		"a\nb":       {"a\n", "b"},
		"a\n\nb\n":   {"a\n", "\n", "b\n"},
		"\n":         {"\n"},
		"a\r\nb\r\n": {"a\r\n", "b\r\n"},
	}
	for in, want := range tests {
		var got []string
		for _, l := range Lines([]byte(in)) {
			got = append(got, string(l))
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("Lines(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsBinary(t *testing.T) {
	t.Parallel()
	if IsBinary([]byte("text\n")) || !IsBinary([]byte("a\x00b")) {
		t.Error("basic detection")
	}
	late := append(bytes.Repeat([]byte("x"), 8<<10), 0)
	if IsBinary(late) {
		t.Error("NUL after the first 8 KiB counted")
	}
	if IsBinary(nil) {
		t.Error("empty is binary")
	}
}

// lcsLen is the DP longest common subsequence length.
func lcsLen(a, b [][]byte) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if bytes.Equal(a[i], b[j]) {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	return dp[0][0]
}

// check verifies that edits cover a and b in order, that equal runs match,
// and returns the number of inserted plus deleted lines.
func check(t *testing.T, a, b [][]byte, edits []Edit) int {
	t.Helper()
	x, y, cost := 0, 0, 0
	for _, e := range edits {
		if e.OldStart != x || e.NewStart != y {
			t.Fatalf("edit %+v does not continue at (%d, %d)", e, x, y)
		}
		switch e.Op {
		case Equal:
			if e.OldEnd-e.OldStart != e.NewEnd-e.NewStart {
				t.Fatalf("equal run with different lengths: %+v", e)
			}
			for i := 0; i < e.OldEnd-e.OldStart; i++ {
				if !bytes.Equal(a[e.OldStart+i], b[e.NewStart+i]) {
					t.Fatalf("equal run %+v has different lines", e)
				}
			}
		case Delete:
			if e.NewEnd != e.NewStart {
				t.Fatalf("delete with new lines: %+v", e)
			}
			cost += e.OldEnd - e.OldStart
		case Insert:
			if e.OldEnd != e.OldStart {
				t.Fatalf("insert with old lines: %+v", e)
			}
			cost += e.NewEnd - e.NewStart
		}
		x, y = e.OldEnd, e.NewEnd
	}
	if x != len(a) || y != len(b) {
		t.Fatalf("edits end at (%d, %d), want (%d, %d)", x, y, len(a), len(b))
	}
	return cost
}

func randomLines(r *rand.ChaCha8, n int, alphabet string) [][]byte {
	lines := make([][]byte, n)
	for i := range lines {
		lines[i] = []byte{alphabet[int(r.Uint64()>>34)%len(alphabet)], '\n'}
	}
	return lines
}

func intn(r *rand.ChaCha8, n int) int { return int(r.Uint64()>>34) % n }

// 10-testing.md: the edit script is minimal on small random inputs (compared
// with a DP LCS for length ≤ 50).
func TestDiffIsMinimal(t *testing.T) {
	t.Parallel()
	r := rand.NewChaCha8([32]byte{1})
	for range 3000 {
		a := randomLines(r, intn(r, 51), "abc"[:1+intn(r, 3)])
		b := randomLines(r, intn(r, 51), "abcd"[:1+intn(r, 4)])
		cost := check(t, a, b, Diff(a, b))
		if want := len(a) + len(b) - 2*lcsLen(a, b); cost != want {
			t.Fatalf("diff of %q and %q costs %d, minimal is %d", a, b, cost, want)
		}
	}
}

func joinLines(lines [][]byte) []byte { return bytes.Join(lines, nil) }

// mutate makes b from a with random edits (inserts, deletes, replacements).
func mutate(r *rand.ChaCha8, a [][]byte) [][]byte {
	b := append([][]byte(nil), a...)
	for range 1 + intn(r, 8) {
		i := intn(r, len(b)+1)
		switch intn(r, 3) {
		case 0:
			b = append(b[:i], append([][]byte{[]byte(fmt.Sprintf("new %d\n", intn(r, 1000)))}, b[i:]...)...)
		case 1:
			if i < len(b) {
				b = append(b[:i], b[i+1:]...)
			}
		default:
			if i < len(b) {
				b[i] = []byte(fmt.Sprintf("changed %d\n", intn(r, 1000)))
			}
		}
	}
	return b
}

// 10-testing.md: applying diff(a, b) to a yields b — through unified hunks,
// for several context sizes, with and without final newlines.
func TestHunksApply(t *testing.T) {
	t.Parallel()
	r := rand.NewChaCha8([32]byte{2})
	for i := range 2000 {
		a := randomLines(r, intn(r, 80), "abcdefgh")
		b := mutate(r, a)
		ab, bb := joinLines(a), joinLines(b)
		if i%3 == 0 && len(ab) > 0 {
			ab = ab[:len(ab)-1] // a lacks its final newline
		}
		if i%5 == 0 && len(bb) > 0 {
			bb = bb[:len(bb)-1]
		}
		for _, ctx := range []int{0, 1, 3, 10} {
			hunks := Hunks(ab, bb, ctx)
			got, err := Apply(ab, hunks)
			if err != nil || !bytes.Equal(got, bb) {
				t.Fatalf("case %d context %d: Apply = %q, %v; want %q\nhunks: %+v", i, ctx, got, err, bb, hunks)
			}
			for _, h := range hunks {
				if h.OldLines+h.NewLines == 0 {
					t.Fatalf("empty hunk %+v", h)
				}
			}
		}
	}
}

func TestHunksFormat(t *testing.T) {
	t.Parallel()
	render := func(hunks []Hunk) string {
		var b strings.Builder
		for _, h := range hunks {
			b.WriteString(h.Header() + "\n")
			b.Write(h.Body)
		}
		return b.String()
	}
	// Expected outputs are `git diff --no-index -U3` hunks: a gap of exactly
	// 2×context lines between changes joins them into one hunk...
	a := []byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n")
	b := []byte("1\n2\n3\nfour\n5\n6\n7\n8\n9\n10\n11")
	want := "@@ -1,10 +1,11 @@\n 1\n 2\n 3\n-4\n+four\n 5\n 6\n 7\n 8\n 9\n 10\n+11\n" + NoNewline
	if got := render(Hunks(a, b, 3)); got != want {
		t.Errorf("hunks:\n%s\nwant:\n%s", got, want)
	}
	// ...and one more line splits them.
	a2 := []byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\nx\n")
	b2 := []byte("1\n2\n3\nfour\n5\n6\n7\n8\n9\n10\nx\n11")
	var headers []string
	for _, h := range Hunks(a2, b2, 3) {
		headers = append(headers, h.Header())
	}
	if fmt.Sprint(headers) != "[@@ -1,7 +1,7 @@ @@ -9,3 +9,4 @@]" {
		t.Errorf("split hunks = %v", headers)
	}
	if Hunks(a, a, 3) != nil {
		t.Error("equal files produced hunks")
	}
	// New file and deleted file.
	if h := Hunks(nil, []byte("x\n"), 3); len(h) != 1 || h[0].Header() != "@@ -0,0 +1 @@" {
		t.Errorf("new file hunk %+v", h)
	}
	if h := Hunks([]byte("x\ny\n"), nil, 3); len(h) != 1 || h[0].Header() != "@@ -1,2 +0,0 @@" {
		t.Errorf("deleted file hunk %+v", h)
	}
}

func TestApplyRejectsMismatch(t *testing.T) {
	t.Parallel()
	hunks := Hunks([]byte("a\nb\n"), []byte("a\nc\n"), 1)
	if _, err := Apply([]byte("a\nX\n"), hunks); err == nil {
		t.Error("hunks applied to the wrong file")
	}
	if _, err := Apply(nil, []Hunk{{OldStart: 5, OldLines: 1, Body: []byte("-x\n")}}); err == nil {
		t.Error("hunk past the end applied")
	}
	if _, err := Apply([]byte("x\n"), []Hunk{{OldStart: 1, OldLines: 1, Body: []byte("?x\n")}}); err == nil {
		t.Error("bad prefix accepted")
	}
	if _, err := Apply([]byte("x\n"), []Hunk{{OldStart: 1, OldLines: 1, Body: []byte(NoNewline)}}); err == nil {
		t.Error("stray marker accepted")
	}
}

func TestDiffLargeAndCostCap(t *testing.T) {
	t.Parallel()
	r := rand.NewChaCha8([32]byte{3})
	// 20k lines with scattered edits: fast and correct.
	var a [][]byte
	for i := range 20000 {
		a = append(a, []byte(fmt.Sprintf("line %d\n", i)))
	}
	b := mutate(r, a)
	check(t, a, b, Diff(a, b))
	// Files with no line in common skip the search entirely.
	x := randomLines(r, 40000, "abcdefghijklmnopqrstuvwxyz")
	y := randomLines(r, 40000, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	if cost := check(t, x, y, Diff(x, y)); cost != 80000 {
		t.Errorf("unrelated files cost %d, want 80000", cost)
	}
	// Over the cost cap a region is replaced wholesale: valid, not minimal.
	p := randomLines(r, 300, "abcd")
	q := randomLines(r, 300, "abcd")
	ip, iq := intern(p, q)
	var matches []pair
	lcs(ip, iq, 0, 0, 3, &matches)
	capped := check(t, p, q, toEdits(matches, len(p), len(q)))
	minimal := check(t, p, q, Diff(p, q))
	if capped < minimal {
		t.Errorf("capped diff (%d) cheaper than the minimal one (%d)", capped, minimal)
	}
	got, err := Apply(joinLines(x), Hunks(joinLines(x), joinLines(y), 3))
	if err != nil || !bytes.Equal(got, joinLines(y)) {
		t.Error("diff of unrelated files does not apply")
	}
}
