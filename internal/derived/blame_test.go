package derived

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/delefrati/xylem/internal/hash"
)

func TestBlameEncodingRoundTrip(t *testing.T) {
	t.Parallel()
	r := newRNG(4)
	ids := []hash.ID{{1}, {2}, {3}}
	for range 500 {
		var lines []BlameLine
		for range r.intn(60) {
			lines = append(lines, BlameLine{Origin: ids[r.intn(3)], Line: 1 + r.intn(40)})
		}
		got, err := DecodeBlame(EncodeBlame(lines))
		if err != nil || !slices.Equal(got, lines) {
			t.Fatalf("round trip of %v = %v, %v", lines, got, err)
		}
	}
	// Runs compress consecutive lines from one origin.
	fresh := FreshBlame(hash.ID{9}, 1000)
	if enc := EncodeBlame(fresh); len(enc) > 1+32+1+6 {
		t.Errorf("1,000 fresh lines encode to %d bytes; want a single run", len(enc))
	}
	if got, err := DecodeBlame(EncodeBlame(nil)); err != nil || len(got) != 0 {
		t.Errorf("empty blame: %v, %v", got, err)
	}
}

func TestDecodeBlameRejects(t *testing.T) {
	t.Parallel()
	id := hash.ID{1}
	good := EncodeBlame([]BlameLine{{id, 1}, {id, 2}})
	for name, b := range map[string][]byte{
		"empty":        nil,
		"truncated":    good[:len(good)-1],
		"trailing":     append(append([]byte{}, good...), 0),
		"origin index": append(append([]byte{1}, id[:]...), 1, 1, 5, 1),
		"zero start":   append(append([]byte{1}, id[:]...), 1, 1, 0, 0),
		"zero count":   append(append([]byte{1}, id[:]...), 1, 0, 0, 1),
	} {
		if _, err := DecodeBlame(b); !errors.Is(err, errBadBlame) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestNextBlame(t *testing.T) {
	t.Parallel()
	a, b := hash.ID{0xa}, hash.ID{0xb}
	prev := FreshBlame(a, 4)
	old := []byte("one\ntwo\nthree\nfour\n")
	cur := []byte("zero\none\nTWO\nthree\nfour")
	got, err := NextBlame(prev, old, cur, b)
	if err != nil {
		t.Fatal(err)
	}
	want := []BlameLine{{b, 1}, {a, 1}, {b, 3}, {a, 3}, {b, 5}} // "four" lost its newline: a changed line
	if !slices.Equal(got, want) {
		t.Errorf("NextBlame = %v, want %v", got, want)
	}
	if _, err := NextBlame(prev, []byte("short\n"), cur, b); err == nil {
		t.Error("mismatched previous blame accepted")
	}
	// Property: every unchanged line keeps its origin, every other line is cur's.
	r := newRNG(6)
	for range 200 {
		var old, cur []byte
		for i := range r.intn(30) {
			old = fmt.Appendf(old, "line %d\n", i%7)
		}
		for i := range r.intn(30) {
			cur = fmt.Appendf(cur, "line %d\n", (i*3)%9)
		}
		prev := FreshBlame(a, len(splitLines(old)))
		got, err := NextBlame(prev, old, cur, b)
		if err != nil || len(got) != len(splitLines(cur)) {
			t.Fatalf("NextBlame: %d lines, %v", len(got), err)
		}
		for i, l := range got {
			if l.Origin == b && l.Line != i+1 {
				t.Fatalf("new line %d attributed to line %d", i+1, l.Line)
			}
			if l.Origin == a && string(splitLines(old)[l.Line-1]) != string(splitLines(cur)[i]) {
				t.Fatalf("inherited line %d differs from its origin", i+1)
			}
		}
	}
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := slices.Index(b, '\n')
		if i < 0 {
			return append(out, b)
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}
