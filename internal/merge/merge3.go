package merge

import (
	"bytes"
	"errors"

	"github.com/delefrati/xylem/internal/diff"
)

// ErrBinary reports a binary input: binary files always conflict as a whole.
var ErrBinary = errors.New("binary file")

// Region is a conflicting region: line ranges (0-based, half-open) in base,
// ours and theirs that both sides changed differently.
type Region struct {
	BaseStart, BaseEnd     int
	OursStart, OursEnd     int
	TheirsStart, TheirsEnd int
}

// Conflict markers written into the merged output around a conflict.
const (
	MarkerOurs   = "<<<<<<< ours\n"
	MarkerSep    = "=======\n"
	MarkerTheirs = ">>>>>>> theirs\n"
)

// change replaces base[baseStart:baseEnd] with side[sideStart:sideEnd].
type change struct {
	baseStart, baseEnd int
	sideStart, sideEnd int
}

// changes groups an edit script into changes (runs between equal lines).
func changes(edits []diff.Edit) []change {
	var out []change
	for _, e := range edits {
		if e.Op == diff.Equal {
			continue
		}
		if n := len(out); n > 0 && out[n-1].baseEnd == e.OldStart && out[n-1].sideEnd == e.NewStart {
			out[n-1].baseEnd, out[n-1].sideEnd = e.OldEnd, e.NewEnd
			continue
		}
		out = append(out, change{e.OldStart, e.OldEnd, e.NewStart, e.NewEnd})
	}
	return out
}

// side is one side of the merge: its lines and its changes from base.
type side struct {
	lines   [][]byte
	changes []change
	next    int // first change not consumed yet
}

// render returns the side's version of base[start:end), which must cover
// every change of the side that touches it (changes[from:to]).
func (s *side) render(base [][]byte, start, end, from, to int) (lines [][]byte, sideStart, sideEnd int) {
	// Map start to the side: offset by the changes before it.
	delta := 0
	for _, c := range s.changes[:from] {
		delta += (c.sideEnd - c.sideStart) - (c.baseEnd - c.baseStart)
	}
	sideStart = start + delta
	pos := start
	for _, c := range s.changes[from:to] {
		lines = append(lines, base[pos:c.baseStart]...)
		lines = append(lines, s.lines[c.sideStart:c.sideEnd]...)
		pos = c.baseEnd
	}
	lines = append(lines, base[pos:end]...)
	return lines, sideStart, sideStart + len(lines)
}

// Merge3 merges ours and theirs, two edits of base, line by line (diff3).
// Changes that don't overlap are both applied; changes that overlap or touch
// (no unchanged base line between them) conflict unless both sides made the
// identical change. Lines keep their terminators, so a missing final newline
// is preserved. On conflict, merged holds the clean parts and each conflict
// between git-style markers, and regions lists the conflicts. Binary inputs
// return ErrBinary.
func Merge3(base, ours, theirs []byte) (merged []byte, regions []Region, err error) {
	if diff.IsBinary(base) || diff.IsBinary(ours) || diff.IsBinary(theirs) {
		return nil, nil, ErrBinary
	}
	b := diff.Lines(base)
	o := &side{lines: diff.Lines(ours)}
	t := &side{lines: diff.Lines(theirs)}
	o.changes = changes(diff.Diff(b, o.lines))
	t.changes = changes(diff.Diff(b, t.lines))

	var out bytes.Buffer
	write := func(lines [][]byte) {
		for _, l := range lines {
			out.Write(l)
		}
	}
	pos := 0 // base lines before pos are written
	for o.next < len(o.changes) || t.next < len(t.changes) {
		// Start a group at the earliest pending change, then absorb every
		// change from either side that overlaps or touches it.
		start := len(b) + 1
		oFrom, tFrom := o.next, t.next
		first := func(s *side) {
			if s.next < len(s.changes) && s.changes[s.next].baseStart < start {
				start = s.changes[s.next].baseStart
			}
		}
		first(o)
		first(t)
		end := start
		for grew := true; grew; {
			grew = false
			for _, s := range []*side{o, t} {
				for s.next < len(s.changes) && s.changes[s.next].baseStart <= end {
					end = max(end, s.changes[s.next].baseEnd)
					s.next++
					grew = true
				}
			}
		}
		write(b[pos:start])
		pos = end
		ol, os, oe := o.render(b, start, end, oFrom, o.next)
		tl, ts, te := t.render(b, start, end, tFrom, t.next)
		switch {
		case o.next == oFrom: // only theirs changed here
			write(tl)
		case t.next == tFrom: // only ours changed here
			write(ol)
		case equalLines(ol, tl): // the same change on both sides
			write(ol)
		default:
			regions = append(regions, Region{start, end, os, oe, ts, te})
			out.WriteString(MarkerOurs)
			writeEnded(&out, ol)
			out.WriteString(MarkerSep)
			writeEnded(&out, tl)
			out.WriteString(MarkerTheirs)
		}
	}
	write(b[pos:])
	return out.Bytes(), regions, nil
}

// writeEnded writes lines inside conflict markers, ending the last one with
// a newline so the next marker starts on its own line.
func writeEnded(out *bytes.Buffer, lines [][]byte) {
	for _, l := range lines {
		out.Write(l)
	}
	if n := len(lines); n > 0 && !bytes.HasSuffix(lines[n-1], []byte("\n")) {
		out.WriteByte('\n')
	}
}

func equalLines(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
