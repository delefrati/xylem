package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/delefrati/xylem/internal/hash"
)

func idOf(b byte) hash.ID {
	var id hash.ID
	for i := range id {
		id[i] = b
	}
	return id
}

func TestMode(t *testing.T) {
	t.Parallel()
	want := map[Mode]string{ModeFile: "file", ModeExec: "exec", ModeSymlink: "symlink", ModeTree: "tree"}
	for m, name := range want {
		if !m.Valid() || m.String() != name {
			t.Errorf("Mode %d: Valid=%v String=%q", m, m.Valid(), m.String())
		}
	}
	// u8 values are hash-affecting (03 §File modes).
	if ModeFile != 1 || ModeExec != 2 || ModeSymlink != 3 || ModeTree != 4 {
		t.Error("mode byte values changed")
	}
	for _, m := range []Mode{0, 5, 255} {
		if m.Valid() || m.String() == "" {
			t.Errorf("Mode %d: Valid=%v String=%q", m, m.Valid(), m.String())
		}
	}
}

// The byte layout written out by hand, independent of the encoder.
func TestEncodeTreeLayout(t *testing.T) {
	t.Parallel()
	tree := &Tree{Entries: []Entry{
		{Mode: ModeFile, Name: []byte("a"), ID: idOf(0x11), Size: 5},
		{Mode: ModeTree, Name: []byte("dir"), ID: idOf(0x22)},
		{Mode: ModeExec, Name: []byte("x"), ID: idOf(0x33), Size: 300},
	}}
	var want []byte
	want = append(want, 3)            // entry_count
	want = append(want, 0x01, 1, 'a') // mode FILE, name "a"
	want = append(want, bytes.Repeat([]byte{0x11}, 32)...)
	want = append(want, 5)                      // size 5
	want = append(want, 0x04, 3, 'd', 'i', 'r') // mode TREE, name "dir"
	want = append(want, bytes.Repeat([]byte{0x22}, 32)...)
	// no size for TREE
	want = append(want, 0x02, 1, 'x') // mode EXEC, name "x"
	want = append(want, bytes.Repeat([]byte{0x33}, 32)...)
	want = append(want, 0xac, 0x02) // size 300 as uvarint

	got, err := EncodeTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("EncodeTree layout mismatch (-want +got):\n%s", diff)
	}
	empty, err := EncodeTree(&Tree{})
	if err != nil || !bytes.Equal(empty, []byte{0}) {
		t.Errorf("EncodeTree(empty) = %x, %v; want 00", empty, err)
	}
}

func TestTreeRoundTrip(t *testing.T) {
	t.Parallel()
	tests := map[string][]Entry{
		"empty": nil,
		"every mode": {
			{Mode: ModeFile, Name: []byte("file"), ID: idOf(1), Size: 0},
			{Mode: ModeSymlink, Name: []byte("link"), ID: idOf(3), Size: 7},
			{Mode: ModeExec, Name: []byte("run.sh"), ID: idOf(2), Size: 1 << 20},
			{Mode: ModeTree, Name: []byte("sub"), ID: idOf(4)},
		},
		// Bytewise order, not git's: "a-b" < "a.b" < "a0" < "aB" < "ab" < "a\xff".
		"sorting edge cases": {
			{Mode: ModeFile, Name: []byte("A"), ID: idOf(1)},
			{Mode: ModeFile, Name: []byte("a"), ID: idOf(2)},
			{Mode: ModeFile, Name: []byte("a-b"), ID: idOf(3)},
			{Mode: ModeTree, Name: []byte("a.b"), ID: idOf(4)},
			{Mode: ModeFile, Name: []byte("a0"), ID: idOf(5)},
			{Mode: ModeFile, Name: []byte("aB"), ID: idOf(6)},
			{Mode: ModeTree, Name: []byte("ab"), ID: idOf(7)},
			{Mode: ModeFile, Name: []byte("a\xff"), ID: idOf(8)},
		},
		"max size":  {{Mode: ModeFile, Name: []byte("big"), ID: idOf(9), Size: ^uint64(0)}},
		"long name": {{Mode: ModeFile, Name: bytes.Repeat([]byte("n"), 300), ID: idOf(10)}},
	}
	for name, entries := range tests {
		tree := &Tree{Entries: entries}
		body, err := EncodeTree(tree)
		if err != nil {
			t.Fatalf("%s: EncodeTree: %v", name, err)
		}
		got, err := DecodeTree(body)
		if err != nil {
			t.Fatalf("%s: DecodeTree: %v", name, err)
		}
		if diff := cmp.Diff(tree, got, cmp.Comparer(func(a, b []Entry) bool {
			return slices.EqualFunc(a, b, entryEqual)
		})); diff != "" {
			t.Errorf("%s: round trip mismatch (-want +got):\n%s", name, diff)
		}
	}
}

func entryEqual(a, b Entry) bool {
	return a.Mode == b.Mode && bytes.Equal(a.Name, b.Name) && a.ID == b.ID && a.Size == b.Size
}

func TestEncodeTreeRejects(t *testing.T) {
	t.Parallel()
	f := func(name string) Entry { return Entry{Mode: ModeFile, Name: []byte(name), ID: idOf(1)} }
	tests := map[string][]Entry{
		"unsorted":        {f("b"), f("a")},
		"duplicate":       {f("a"), f("a")},
		"duplicate mixed": {f("a"), {Mode: ModeTree, Name: []byte("a"), ID: idOf(2)}},
		"empty name":      {f("")},
		"slash":           {f("a/b")},
		"NUL":             {f("a\x00")},
		"dot":             {f(".")},
		"dotdot":          {f("..")},
		"reserved":        {f(".xylem")},
		"mode 0":          {{Mode: 0, Name: []byte("a"), ID: idOf(1)}},
		"mode 5":          {{Mode: 5, Name: []byte("a"), ID: idOf(1)}},
		"tree with size":  {{Mode: ModeTree, Name: []byte("a"), ID: idOf(1), Size: 1}},
	}
	for name, entries := range tests {
		if _, err := EncodeTree(&Tree{Entries: entries}); !errors.Is(err, ErrInvalidTree) {
			t.Errorf("%s: EncodeTree error = %v, want ErrInvalidTree", name, err)
		}
	}
}

func TestDecodeTreeRejects(t *testing.T) {
	t.Parallel()
	id := bytes.Repeat([]byte{7}, 32)
	entry := func(mode byte, name string, size ...byte) []byte {
		b := binary.AppendUvarint([]byte{mode}, uint64(len(name)))
		b = append(b, name...)
		b = append(b, id...)
		return append(b, size...)
	}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	tests := map[string][]byte{
		"empty input":             nil,
		"trailing bytes":          cat([]byte{1}, entry(1, "a", 0), []byte{0}),
		"trailing after empty":    {0, 0},
		"count too high":          cat([]byte{2}, entry(1, "a", 0)),
		"non-minimal count":       cat([]byte{0x81, 0x00}, entry(1, "a", 0)),
		"non-minimal empty count": {0x80, 0x00},
		"non-minimal name length": cat([]byte{1, 1, 0x81, 0x00, 'a'}, id, []byte{0}),
		"non-minimal size":        cat([]byte{1}, entry(1, "a", 0x85, 0x00)),
		"overflowing size":        cat([]byte{1}, entry(1, "a", 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02)),
		"name past end":           {1, 1, 50, 'a'},
		"truncated id":            cat([]byte{1, 1, 1, 'a'}, id[:31]),
		"missing size":            cat([]byte{1, 1, 1, 'a'}, id),
		"mode 0":                  cat([]byte{1}, entry(0, "a", 0)),
		"mode 5":                  cat([]byte{1}, entry(5, "a", 0)),
		"unsorted":                cat([]byte{2}, entry(1, "b", 0), entry(1, "a", 0)),
		"duplicate":               cat([]byte{2}, entry(1, "a", 0), entry(4, "a")),
		"empty name":              cat([]byte{1}, entry(1, "", 0)),
		"reserved name":           cat([]byte{1}, entry(4, ".xylem")),
		"slash in name":           cat([]byte{1}, entry(1, "a/b", 0)),
		"huge count":              {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01},
	}
	for name, body := range tests {
		if tree, err := DecodeTree(body); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: DecodeTree = %v, %v; want ErrCorrupt", name, tree, err)
		}
	}
}

func TestDecodeTreeRejectsEveryTruncation(t *testing.T) {
	t.Parallel()
	body, err := EncodeTree(&Tree{Entries: []Entry{
		{Mode: ModeFile, Name: []byte("a"), ID: idOf(1), Size: 300},
		{Mode: ModeTree, Name: []byte("b"), ID: idOf(2)},
		{Mode: ModeSymlink, Name: []byte("c"), ID: idOf(3), Size: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for n := range len(body) {
		if _, err := DecodeTree(body[:n]); !errors.Is(err, ErrCorrupt) {
			t.Errorf("DecodeTree(first %d of %d bytes) error = %v, want ErrCorrupt", n, len(body), err)
		}
	}
}

func TestTreeRandomRoundTrip(t *testing.T) {
	t.Parallel()
	src := rand.NewChaCha8([32]byte{3})
	randByte := func() byte {
		var b [1]byte
		_, _ = src.Read(b[:])
		return b[0]
	}
	for range 300 {
		names := map[string]bool{}
		for range int(randByte() % 40) {
			name := make([]byte, 1+randByte()%6)
			for i := range name {
				name[i] = "ab.-0\xff/"[randByte()%7]
			}
			if !bytes.Contains(name, []byte("/")) && string(name) != "." && string(name) != ".." {
				names[string(name)] = true
			}
		}
		var tree Tree
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for n := range names {
				if !yield(n) {
					return
				}
			}
		}) {
			e := Entry{Mode: Mode(1 + randByte()%4), Name: []byte(name)}
			_, _ = src.Read(e.ID[:])
			if e.Mode != ModeTree {
				e.Size = uint64(randByte()) << (randByte() % 50)
			}
			tree.Entries = append(tree.Entries, e)
		}
		body, err := EncodeTree(&tree)
		if err != nil {
			t.Fatalf("EncodeTree: %v", err)
		}
		got, err := DecodeTree(body)
		if err != nil {
			t.Fatalf("DecodeTree: %v", err)
		}
		again, err := EncodeTree(got)
		if err != nil || !bytes.Equal(again, body) {
			t.Fatalf("re-encode differs: %v", err)
		}
	}
}

func TestTreeLookup(t *testing.T) {
	t.Parallel()
	tree := &Tree{Entries: []Entry{
		{Mode: ModeFile, Name: []byte("a"), ID: idOf(1), Size: 1},
		{Mode: ModeTree, Name: []byte("b"), ID: idOf(2)},
		{Mode: ModeExec, Name: []byte("d"), ID: idOf(3), Size: 3},
	}}
	for _, e := range tree.Entries {
		got, ok := tree.Lookup(e.Name)
		if !ok || !entryEqual(got, e) {
			t.Errorf("Lookup(%q) = %v, %v; want %v", e.Name, got, ok, e)
		}
	}
	for _, name := range []string{"", "0", "c", "bb", "e", "A"} {
		if got, ok := tree.Lookup([]byte(name)); ok {
			t.Errorf("Lookup(%q) = %v, want not found", name, got)
		}
	}
	if _, ok := (&Tree{}).Lookup([]byte("a")); ok {
		t.Error("Lookup on empty tree found something")
	}
}
