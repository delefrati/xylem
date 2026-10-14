package diff

import (
	"bytes"
	"errors"
	"fmt"
)

// NoNewline marks a line that has no terminating newline in unified output.
const NoNewline = "\\ No newline at end of file\n"

// Hunk is one unified-diff hunk. Starts are 1-based; when a side has no
// lines, its start is the line before the hunk (0 at the top), as in git.
// Body holds the lines prefixed with ' ', '-' or '+', each ending in '\n';
// a line without a newline in its file is followed by NoNewline.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	Body               []byte
}

// Header returns the "@@ -a,b +c,d @@" line (without newline).
func (h Hunk) Header() string {
	return fmt.Sprintf("@@ -%s +%s @@", rng(h.OldStart, h.OldLines), rng(h.NewStart, h.NewLines))
}

func rng(start, n int) string {
	if n == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, n)
}

// Hunks diffs two files line by line and groups the changes into unified
// hunks with context lines of context (changes closer than 2*context lines
// share a hunk). It returns nil if the files are equal.
func Hunks(a, b []byte, context int) []Hunk {
	la, lb := Lines(a), Lines(b)
	edits := Diff(la, lb)
	context = max(context, 0)

	var hunks []Hunk
	for i := 0; i < len(edits); {
		if edits[i].Op == Equal {
			i++
			continue
		}
		// A hunk starts at change i and grows while the equal runs between
		// changes are short enough to join.
		j := i
		for {
			for j+1 < len(edits) && edits[j+1].Op != Equal { // e.g. a delete followed by an insert
				j++
			}
			if j+2 < len(edits) && edits[j+1].OldEnd-edits[j+1].OldStart <= 2*context {
				j += 2 // a short equal run: the next change joins this hunk
				continue
			}
			break
		}
		oldFrom := max(edits[i].OldStart-context, 0)
		newFrom := max(edits[i].NewStart-context, 0)
		oldTo := min(edits[j].OldEnd+context, len(la))
		newTo := min(edits[j].NewEnd+context, len(lb))

		var body bytes.Buffer
		write := func(prefix byte, line []byte) {
			body.WriteByte(prefix)
			body.Write(line)
			if !bytes.HasSuffix(line, []byte{'\n'}) {
				body.WriteString("\n" + NoNewline)
			}
		}
		x, y := oldFrom, newFrom
		for k := i; k <= j; k++ {
			e := edits[k]
			for ; x < e.OldStart; x, y = x+1, y+1 { // leading context / equal run
				write(' ', la[x])
			}
			switch e.Op {
			case Equal:
				for ; x < e.OldEnd; x, y = x+1, y+1 {
					write(' ', la[x])
				}
			case Delete:
				for ; x < e.OldEnd; x++ {
					write('-', la[x])
				}
			case Insert:
				for ; y < e.NewEnd; y++ {
					write('+', lb[y])
				}
			}
		}
		for ; x < oldTo && y < newTo; x, y = x+1, y+1 { // trailing context
			write(' ', la[x])
		}
		h := Hunk{OldLines: oldTo - oldFrom, NewLines: newTo - newFrom, Body: body.Bytes()}
		h.OldStart, h.NewStart = oldFrom+1, newFrom+1
		if h.OldLines == 0 {
			h.OldStart = oldFrom
		}
		if h.NewLines == 0 {
			h.NewStart = newFrom
		}
		hunks = append(hunks, h)
		i = j + 1
	}
	return hunks
}

// ErrMismatch reports hunks that do not apply to the given file.
var ErrMismatch = errors.New("hunk does not apply")

// hunkLine is one parsed body line: its prefix and the line as it appears in
// the file (without the trailing newline if the file has none there).
type hunkLine struct {
	prefix byte
	line   []byte
}

func parseBody(body []byte) ([]hunkLine, error) {
	var out []hunkLine
	for len(body) > 0 {
		nl := bytes.IndexByte(body, '\n')
		if nl < 0 {
			return nil, errors.New("body is not newline-terminated")
		}
		raw := body[:nl+1]
		body = body[nl+1:]
		if string(raw) == NoNewline {
			if len(out) == 0 {
				return nil, errors.New("stray no-newline marker")
			}
			last := &out[len(out)-1]
			last.line = bytes.TrimSuffix(last.line, []byte{'\n'})
			continue
		}
		switch raw[0] {
		case ' ', '-', '+':
			out = append(out, hunkLine{prefix: raw[0], line: raw[1:]})
		default:
			return nil, fmt.Errorf("bad line prefix %q", raw[0])
		}
	}
	return out, nil
}

// Apply applies unified hunks (in order) to a and returns the new file.
func Apply(a []byte, hunks []Hunk) ([]byte, error) {
	la := Lines(a)
	var out bytes.Buffer
	pos := 0 // next unconsumed line of a (0-based)
	for hi, h := range hunks {
		start := h.OldStart - 1
		if h.OldLines == 0 {
			start = h.OldStart
		}
		if start < pos || start > len(la) {
			return nil, fmt.Errorf("%w: hunk %d starts at old line %d", ErrMismatch, hi, h.OldStart)
		}
		for ; pos < start; pos++ {
			out.Write(la[pos])
		}
		lines, err := parseBody(h.Body)
		if err != nil {
			return nil, fmt.Errorf("%w: hunk %d: %w", ErrMismatch, hi, err)
		}
		for _, l := range lines {
			if l.prefix == '+' {
				out.Write(l.line)
				continue
			}
			if pos >= len(la) || !bytes.Equal(la[pos], l.line) {
				return nil, fmt.Errorf("%w: hunk %d: old line %d differs", ErrMismatch, hi, pos+1)
			}
			if l.prefix == ' ' {
				out.Write(la[pos])
			}
			pos++
		}
	}
	for ; pos < len(la); pos++ {
		out.Write(la[pos])
	}
	return out.Bytes(), nil
}
