package derived

import (
	"errors"
	"fmt"

	"github.com/delefrati/xylem/internal/canon"
	"github.com/delefrati/xylem/internal/diff"
	"github.com/delefrati/xylem/internal/hash"
)

// BlameKind is kind 4 (04 §Kind 4): blame of a path at a changeset that
// modifies it, following first-parent history. Version 2 (M6.9): the line
// diff compacts change runs like git, which changes attributions; values
// stored by version 1 are ignored.
var BlameKind = Kind{Name: "blame", Version: 2}

// BlameLine attributes one line: the changeset that introduced it and its
// 1-based line number in that changeset's version of the file.
type BlameLine struct {
	Origin hash.ID
	Line   int
}

// FreshBlame attributes all n lines to id.
func FreshBlame(id hash.ID, n int) []BlameLine {
	out := make([]BlameLine, n)
	for i := range out {
		out[i] = BlameLine{Origin: id, Line: i + 1}
	}
	return out
}

// NextBlame computes the blame of cur (content curContent) from the blame of
// the previous version: lines unchanged by the line diff keep their origin;
// inserted or replaced lines are attributed to cur.
func NextBlame(prev []BlameLine, prevContent, curContent []byte, cur hash.ID) ([]BlameLine, error) {
	a, b := diff.Lines(prevContent), diff.Lines(curContent)
	if len(a) != len(prev) {
		return nil, fmt.Errorf("blame has %d lines, previous content %d", len(prev), len(a))
	}
	out := make([]BlameLine, len(b))
	for _, e := range diff.Diff(a, b) {
		switch e.Op {
		case diff.Equal:
			copy(out[e.NewStart:e.NewEnd], prev[e.OldStart:e.OldEnd])
		case diff.Insert:
			for i := e.NewStart; i < e.NewEnd; i++ {
				out[i] = BlameLine{Origin: cur, Line: i + 1}
			}
		}
	}
	return out, nil
}

// EncodeBlame serializes per-line blame (04 §Kind 4, version 1):
//
//	uvarint(origin_count) ‖ id* ‖
//	uvarint(run_count) ‖ (uvarint(line_count) ‖ uvarint(origin_index) ‖ uvarint(origin_start_line))*
//
// A run is consecutive lines from one origin with consecutive origin lines.
func EncodeBlame(lines []BlameLine) []byte {
	index := map[hash.ID]int{}
	var origins []hash.ID
	type run struct{ count, origin, start int }
	var runs []run
	prevLine := 0 // origin line of the previous line in the current run
	for _, l := range lines {
		idx, ok := index[l.Origin]
		if !ok {
			idx = len(origins)
			index[l.Origin] = idx
			origins = append(origins, l.Origin)
		}
		if n := len(runs); n > 0 && runs[n-1].origin == idx && prevLine+1 == l.Line {
			runs[n-1].count++
		} else {
			runs = append(runs, run{1, idx, l.Line})
		}
		prevLine = l.Line
	}
	b := appendUint(nil, len(origins))
	for _, o := range origins {
		b = append(b, o[:]...)
	}
	b = appendUint(b, len(runs))
	for _, r := range runs {
		b = appendUint(appendUint(appendUint(b, r.count), r.origin), r.start)
	}
	return b
}

func appendUint(b []byte, v int) []byte {
	var tmp [10]byte
	n := 0
	u := uint64(max(v, 0))
	for u >= 0x80 {
		tmp[n] = byte(u) | 0x80
		u >>= 7
		n++
	}
	tmp[n] = byte(u)
	return append(b, tmp[:n+1]...)
}

// errBadBlame reports a malformed blame value.
var errBadBlame = errors.New("malformed blame value")

// DecodeBlame parses a value written by EncodeBlame.
func DecodeBlame(b []byte) ([]BlameLine, error) {
	r := canon.NewReader(b)
	n, err := r.Uvarint()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBadBlame, err)
	}
	origins := make([]hash.ID, 0, r.CapHint(n, hash.Size))
	for range n {
		id, err := r.ID()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errBadBlame, err)
		}
		origins = append(origins, id)
	}
	runs, err := r.Uvarint()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBadBlame, err)
	}
	var lines []BlameLine
	for range runs {
		count, err1 := r.Uvarint()
		idx, err2 := r.Uvarint()
		start, err3 := r.Uvarint()
		if err := errors.Join(err1, err2, err3); err != nil {
			return nil, fmt.Errorf("%w: %w", errBadBlame, err)
		}
		if idx >= uint64(len(origins)) || start == 0 || count == 0 || count > 1<<31 || start > 1<<31 {
			return nil, fmt.Errorf("%w: run out of range", errBadBlame)
		}
		for i := range int(count) {
			lines = append(lines, BlameLine{Origin: origins[idx], Line: int(start) + i})
		}
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("%w: trailing bytes", errBadBlame)
	}
	return lines, nil
}
