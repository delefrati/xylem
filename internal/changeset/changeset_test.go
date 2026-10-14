package changeset

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
)

func idOf(b byte) hash.ID {
	var id hash.ID
	for i := range id {
		id[i] = b
	}
	return id
}

func put(path string, mode object.Mode, size uint64, blob byte) Change {
	return Change{Path: []byte(path), Op: OpPut, Mode: mode, Size: size, Blob: idOf(blob)}
}

func del(path string) Change {
	return Change{Path: []byte(path), Op: OpDelete}
}

func sig(name string, t int64, tz int32) Signature {
	return Signature{NameEmail: []byte(name), Time: t, TZMinutes: tz}
}

// The byte layout written out by hand, independent of the encoder.
func TestEncodeLayout(t *testing.T) {
	t.Parallel()
	c := &Changeset{
		Parents:   []hash.ID{idOf(0xaa)},
		Author:    sig("A <a@x>", 1000, -180),
		Committer: sig("C <c@x>", 1001, 60),
		Message:   []byte("msg\n"),
		Extras:    map[string][]byte{"k2": []byte("v2"), "k1": []byte("v1")},
		Changes:   []Change{put("b", object.ModeExec, 300, 0xbb), del("a")},
	}
	var want []byte
	want = append(want, 1) // format version
	want = append(want, 1) // parent count
	want = append(want, bytes.Repeat([]byte{0xaa}, 32)...)
	want = append(want, 7) // len("A <a@x>")
	want = append(want, "A <a@x>"...)
	want = append(want, 0xd0, 0x0f) // varint(1000) = zigzag 2000
	want = append(want, 0xe7, 0x02) // varint(-180) = zigzag 359
	want = append(want, 7)
	want = append(want, "C <c@x>"...)
	want = append(want, 0xd2, 0x0f) // varint(1001) = zigzag 2002
	want = append(want, 0x78)       // varint(60) = zigzag 120
	want = append(want, 4)
	want = append(want, "msg\n"...)
	want = append(want, 2) // extra count, sorted by key
	want = append(want, 2, 'k', '1', 2, 'v', '1')
	want = append(want, 2, 'k', '2', 2, 'v', '2')
	want = append(want, 2)                              // change count, sorted by path
	want = append(want, 1, 'a', 0x02)                   // DELETE "a"
	want = append(want, 1, 'b', 0x01, 0x02, 0xac, 0x02) // PUT "b" EXEC size 300
	want = append(want, bytes.Repeat([]byte{0xbb}, 32)...)

	got, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Encode layout mismatch (-want +got):\n%s", diff)
	}
	id, err := c.ID()
	if err != nil || id != object.ID(object.KindChangeset, want) {
		t.Errorf("ID = %s, %v; want object id of the body", id, err)
	}
}

func roundTripCases() map[string]*Changeset {
	return map[string]*Changeset{
		"empty root": {
			Author: sig("Root <r@x>", 0, 0), Committer: sig("Root <r@x>", 0, 0),
		},
		"root with files": {
			Author: sig("A <a@x>", 1700000000, 60), Committer: sig("A <a@x>", 1700000000, 60),
			Message: []byte("initial\n"),
			Changes: []Change{
				put("README", object.ModeFile, 12, 1),
				put("bin/run", object.ModeExec, 99, 2),
				put("link", object.ModeSymlink, 6, 3),
			},
		},
		"merge without changes": {
			Parents: []hash.ID{idOf(1), idOf(2), idOf(3)},
			Author:  sig("M <m@x>", 5, 0), Committer: sig("M <m@x>", 6, 0),
			Message: []byte("Merge\n"),
		},
		"deletes and extras": {
			Parents: []hash.ID{idOf(9)},
			Author:  sig("D <d@x>", -1, -720), Committer: sig("D <d@x>", math.MaxInt64, 840),
			Message: nil,
			Extras: map[string][]byte{
				ExtraGitSHA:      []byte(strings.Repeat("ab", 20)),
				ExtraRebasedFrom: []byte(strings.Repeat("cd", 32)),
				"unknown-extra":  {0, 1, 2},
				"empty-value":    {},
			},
			Changes: []Change{del("old/file"), del("z"), put("new", object.ModeFile, 0, 4)},
		},
		"non-UTF-8": {
			Parents: []hash.ID{idOf(7)},
			Author:  sig("J\xf6rg <j@x>", math.MinInt64, math.MinInt32), Committer: sig("\xff", 1, math.MaxInt32),
			Message: []byte("caf\xe9\x00\n"),
			Changes: []Change{put("dir/\xff\xfe.txt", object.ModeFile, 3, 5)},
		},
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	for name, c := range roundTripCases() {
		body, err := c.Encode()
		if err != nil {
			t.Fatalf("%s: Encode: %v", name, err)
		}
		got, err := Decode(body)
		if err != nil {
			t.Fatalf("%s: Decode: %v", name, err)
		}
		again, err := got.Encode()
		if err != nil || !bytes.Equal(again, body) {
			t.Errorf("%s: re-encode differs: %v", name, err)
		}
		want := *c
		want.Changes = slices.Clone(c.Changes)
		slices.SortFunc(want.Changes, func(a, b Change) int { return bytes.Compare(a.Path, b.Path) })
		if diff := cmp.Diff(&want, got, cmp.Comparer(func(a, b []byte) bool { return bytes.Equal(a, b) })); diff != "" {
			t.Errorf("%s: decoded mismatch (-want +got):\n%s", name, diff)
		}
	}
}

func TestEncodeIsOrderIndependentAndPure(t *testing.T) {
	t.Parallel()
	c := roundTripCases()["deletes and extras"]
	want, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	original := slices.Clone(c.Changes)
	src := rand.NewChaCha8([32]byte{5})
	for range 20 {
		shuffled := *c
		shuffled.Changes = slices.Clone(c.Changes)
		for i := len(shuffled.Changes) - 1; i > 0; i-- { // Fisher-Yates; n is tiny so modulo bias is irrelevant
			var b [1]byte
			_, _ = src.Read(b[:])
			j := int(b[0]) % (i + 1)
			shuffled.Changes[i], shuffled.Changes[j] = shuffled.Changes[j], shuffled.Changes[i]
		}
		order := slices.Clone(shuffled.Changes)
		got, err := shuffled.Encode()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("shuffled changes encode differently: %v", err)
		}
		if diff := cmp.Diff(order, shuffled.Changes); diff != "" {
			t.Fatalf("Encode reordered its input:\n%s", diff)
		}
	}
	if diff := cmp.Diff(original, c.Changes); diff != "" {
		t.Errorf("Encode modified c.Changes:\n%s", diff)
	}
}

func TestEncodeRejects(t *testing.T) {
	t.Parallel()
	tests := map[string][]Change{
		"duplicate path":     {put("a", object.ModeFile, 1, 1), del("a")},
		"PUT with tree mode": {put("a", object.ModeTree, 0, 1)},
		"PUT with zero mode": {put("a", 0, 1, 1)},
		"PUT with mode 9":    {put("a", 9, 1, 1)},
		"DELETE with mode":   {{Path: []byte("a"), Op: OpDelete, Mode: object.ModeFile}},
		"DELETE with size":   {{Path: []byte("a"), Op: OpDelete, Size: 1}},
		"DELETE with blob":   {{Path: []byte("a"), Op: OpDelete, Blob: idOf(1)}},
		"zero op":            {{Path: []byte("a")}},
		"unknown op":         {{Path: []byte("a"), Op: 3}},
	}
	for name, changes := range tests {
		c := &Changeset{Parents: []hash.ID{idOf(1)}, Changes: changes}
		if _, err := c.Encode(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Encode error = %v, want ErrInvalid", name, err)
		}
		if _, err := c.ID(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: ID error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	t.Parallel()
	// header builds a valid prefix up to and including the message.
	header := func(version uint64) []byte {
		b := binary.AppendUvarint(nil, version)
		b = append(b, 0)            // no parents
		b = append(b, 1, 'a', 0, 0) // author "a", time 0, tz 0
		b = append(b, 1, 'c', 0, 0) // committer
		return append(b, 0)         // empty message
	}
	h := header(1)
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	blob := bytes.Repeat([]byte{1}, 32)
	putA := cat([]byte{1, 'a', 0x01, 0x01, 0}, blob)
	putB := cat([]byte{1, 'b', 0x01, 0x01, 0}, blob)
	tests := map[string][]byte{
		"empty":                 nil,
		"version 0":             cat(header(0), []byte{0, 0}),
		"version 2":             cat(header(2), []byte{0, 0}),
		"non-minimal version":   cat([]byte{0x81, 0x00}, h[1:], []byte{0, 0}),
		"missing counts":        h,
		"trailing bytes":        cat(h, []byte{0, 0, 0}),
		"too many parents":      cat([]byte{1, 5}, bytes.Repeat([]byte{1}, 64)),
		"huge parent count":     cat([]byte{1, 0xff, 0xff, 0xff, 0xff, 0x0f}),
		"tz above int32":        cat([]byte{1, 0, 1, 'a', 0}, binary.AppendVarint(nil, math.MaxInt32+1), []byte{1, 'c', 0, 0, 0, 0, 0}),
		"tz below int32":        cat([]byte{1, 0, 1, 'a', 0}, binary.AppendVarint(nil, math.MinInt32-1), []byte{1, 'c', 0, 0, 0, 0, 0}),
		"non-minimal time":      cat([]byte{1, 0, 1, 'a', 0x80, 0x00, 0, 1, 'c', 0, 0, 0, 0, 0}),
		"unsorted extras":       cat(h, []byte{2, 1, 'b', 0, 1, 'a', 0, 0}),
		"duplicate extras":      cat(h, []byte{2, 1, 'a', 0, 1, 'a', 0, 0}),
		"truncated extra value": cat(h, []byte{1, 1, 'a', 5, 'x'}),
		"unsorted changes":      cat(h, []byte{0, 2}, putB, putA),
		"duplicate changes":     cat(h, []byte{0, 2}, putA, []byte{1, 'a', 0x02}),
		"zero op":               cat(h, []byte{0, 1, 1, 'a', 0x00}),
		"unknown op":            cat(h, []byte{0, 1, 1, 'a', 0x03}),
		"PUT tree mode":         cat(h, []byte{0, 1, 1, 'a', 0x01, 0x04, 0}, blob),
		"PUT zero mode":         cat(h, []byte{0, 1, 1, 'a', 0x01, 0x00, 0}, blob),
		"non-minimal size":      cat(h, []byte{0, 1, 1, 'a', 0x01, 0x01, 0x80, 0x00}, blob),
		"truncated blob":        cat(h, []byte{0, 1, 1, 'a', 0x01, 0x01, 0}, blob[:10]),
		"huge change count":     cat(h, []byte{0, 0xff, 0xff, 0xff, 0xff, 0x0f}),
		"change count too high": cat(h, []byte{0, 2}, putA),
	}
	for name, body := range tests {
		if c, err := Decode(body); !errors.Is(err, object.ErrCorrupt) {
			t.Errorf("%s: Decode = %+v, %v; want ErrCorrupt", name, c, err)
		}
	}
}

func TestDecodeRejectsEveryTruncation(t *testing.T) {
	t.Parallel()
	for name, c := range roundTripCases() {
		body, err := c.Encode()
		if err != nil {
			t.Fatal(err)
		}
		for n := range len(body) {
			if _, err := Decode(body[:n]); !errors.Is(err, object.ErrCorrupt) {
				t.Errorf("%s: Decode(first %d of %d bytes) error = %v, want ErrCorrupt", name, n, len(body), err)
			}
		}
	}
}

func TestValidateShape(t *testing.T) {
	t.Parallel()
	ok := func() *Changeset {
		return &Changeset{Parents: []hash.ID{idOf(1)}, Changes: []Change{put("a", object.ModeFile, 1, 1)}}
	}
	valid := map[string]*Changeset{
		"single parent":     ok(),
		"empty root":        {},
		"root with changes": {Changes: []Change{put("a", object.ModeFile, 1, 1)}},
		"empty merge":       {Parents: []hash.ID{idOf(1), idOf(2)}},
		"16 parents":        {Parents: make([]hash.ID, MaxParents)},
		"max message":       func() *Changeset { c := ok(); c.Message = make([]byte, MaxMessageSize); return c }(),
		"max extra":         func() *Changeset { c := ok(); c.Extras = map[string][]byte{"k": make([]byte, MaxExtraValue)}; return c }(),
		"non-UTF-8 path":    func() *Changeset { c := ok(); c.Changes[0].Path = []byte("\xff/x"); return c }(),
	}
	for name, c := range valid {
		if err := c.ValidateShape(ShapeOptions{}); err != nil {
			t.Errorf("%s: ValidateShape = %v, want nil", name, err)
		}
	}

	invalid := map[string]*Changeset{
		"17 parents":        {Parents: make([]hash.ID, MaxParents+1)},
		"no changes":        {Parents: []hash.ID{idOf(1)}},
		"too many changes":  {Parents: []hash.ID{idOf(1)}, Changes: make([]Change, MaxChanges+1)},
		"message too large": func() *Changeset { c := ok(); c.Message = make([]byte, MaxMessageSize+1); return c }(),
		"extra too large": func() *Changeset {
			c := ok()
			c.Extras = map[string][]byte{"k": make([]byte, MaxExtraValue+1)}
			return c
		}(),
		"empty extra key":    func() *Changeset { c := ok(); c.Extras = map[string][]byte{"": nil}; return c }(),
		"non-UTF-8 key":      func() *Changeset { c := ok(); c.Extras = map[string][]byte{"\xff": nil}; return c }(),
		"empty path":         func() *Changeset { c := ok(); c.Changes[0].Path = nil; return c }(),
		"reserved path":      func() *Changeset { c := ok(); c.Changes[0].Path = []byte("a/.xylem/b"); return c }(),
		"leading slash":      func() *Changeset { c := ok(); c.Changes[0].Path = []byte("/a"); return c }(),
		"duplicate path":     func() *Changeset { c := ok(); c.Changes = append(c.Changes, del("a")); return c }(),
		"PUT with tree mode": func() *Changeset { c := ok(); c.Changes[0].Mode = object.ModeTree; return c }(),
		"DELETE with blob": func() *Changeset {
			c := ok()
			c.Changes = []Change{{Path: []byte("a"), Op: OpDelete, Blob: idOf(1)}}
			return c
		}(),
	}
	for name, c := range invalid {
		if err := c.ValidateShape(ShapeOptions{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: ValidateShape = %v, want ErrInvalid", name, err)
		}
	}

	empty := &Changeset{Parents: []hash.ID{idOf(1)}}
	if err := empty.ValidateShape(ShapeOptions{AllowEmpty: true}); err != nil {
		t.Errorf("empty commit with AllowEmpty: %v", err)
	}
}

func TestSummary(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", MaxSummaryLength+50)
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{"empty", "", ""},
		{"one line", "fix the thing", "fix the thing"},
		{"first line only", "subject\n\nbody text\n", "subject"},
		{"CRLF", "subject\r\nbody", "subject"},
		{"invalid UTF-8", "caf\xe9 au lait", "caf� au lait"},
		{"NUL", "a\x00b", "a�b"},
		{"truncated to 200 runes", long, strings.Repeat("é", MaxSummaryLength)},
		{"exactly 200 runes", strings.Repeat("x", MaxSummaryLength), strings.Repeat("x", MaxSummaryLength)},
		{"leading newline", "\nsubject", ""},
	}
	for _, tt := range tests {
		c := &Changeset{Message: []byte(tt.message)}
		if got := c.Summary(); got != tt.want {
			t.Errorf("%s: Summary() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestOpString(t *testing.T) {
	t.Parallel()
	for op, want := range map[Op]string{OpPut: "PUT", OpDelete: "DELETE", 0: "Op(0)", 7: "Op(7)"} {
		if got := op.String(); got != want {
			t.Errorf("Op(%d).String() = %q, want %q", uint8(op), got, want)
		}
	}
}
