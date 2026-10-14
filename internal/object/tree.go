package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/paths"
)

// Mode is a tree entry's file mode (03-data-model.md §File modes).
type Mode uint8

// File modes. The zero Mode is invalid.
const (
	ModeFile    Mode = 0x01 // git 100644
	ModeExec    Mode = 0x02 // git 100755
	ModeSymlink Mode = 0x03 // git 120000; the blob holds the link target
	ModeTree    Mode = 0x04 // git 040000
)

var modeNames = [...]string{
	ModeFile:    "file",
	ModeExec:    "exec",
	ModeSymlink: "symlink",
	ModeTree:    "tree",
}

// Valid reports whether m is one of the defined modes.
func (m Mode) Valid() bool {
	return m >= ModeFile && m <= ModeTree
}

// String returns "file", "exec", "symlink" or "tree".
func (m Mode) String() string {
	if !m.Valid() {
		return "Mode(" + strconv.Itoa(int(m)) + ")"
	}
	return modeNames[m]
}

// ErrInvalidTree reports a Tree that cannot be encoded.
var ErrInvalidTree = errors.New("invalid tree")

// Entry is one name in a tree. ID is a blob id, or a tree id for ModeTree.
// Size is the blob's length in bytes and must be 0 for ModeTree (ADR-017).
type Entry struct {
	Mode Mode
	Name []byte
	ID   hash.ID
	Size uint64
}

// Tree is a directory listing with entries in strictly ascending bytewise
// order of name. An empty tree is encodable; only a root tree may be empty,
// which is checked by treeops.
type Tree struct {
	Entries []Entry
}

// minEntrySize is the smallest encoded entry: mode, 1-byte name length,
// 1-byte name, id (a tree entry has no size).
const minEntrySize = 1 + 1 + 1 + hash.Size

// EncodeTree returns the canonical body of t:
//
//	uvarint(entry_count) ‖ entry*
//	entry = u8(mode) ‖ uvarint(len(name)) ‖ name ‖ id ‖ [uvarint(size) if mode != TREE]
//
// It fails, wrapping ErrInvalidTree, if a mode or name is invalid, entries are
// not strictly ascending by name, or a ModeTree entry has a non-zero Size.
func EncodeTree(t *Tree) ([]byte, error) {
	size := binary.MaxVarintLen64
	for i, e := range t.Entries {
		if err := checkEntry(e); err != nil {
			return nil, fmt.Errorf("%w: entry %d: %w", ErrInvalidTree, i, err)
		}
		if i > 0 && bytes.Compare(t.Entries[i-1].Name, e.Name) >= 0 {
			return nil, fmt.Errorf("%w: entry %d: name %q not after %q", ErrInvalidTree, i, e.Name, t.Entries[i-1].Name)
		}
		size += 1 + binary.MaxVarintLen64 + len(e.Name) + hash.Size + binary.MaxVarintLen64
	}
	b := make([]byte, 0, size)
	b = binary.AppendUvarint(b, uint64(len(t.Entries)))
	for _, e := range t.Entries {
		b = append(b, byte(e.Mode))
		b = binary.AppendUvarint(b, uint64(len(e.Name)))
		b = append(b, e.Name...)
		b = append(b, e.ID[:]...)
		if e.Mode != ModeTree {
			b = binary.AppendUvarint(b, e.Size)
		}
	}
	return b, nil
}

func checkEntry(e Entry) error {
	if !e.Mode.Valid() {
		return fmt.Errorf("invalid mode %d", e.Mode)
	}
	if err := paths.ValidName(e.Name); err != nil {
		return err
	}
	if e.Mode == ModeTree && e.Size != 0 {
		return fmt.Errorf("tree entry %q has size %d, want 0", e.Name, e.Size)
	}
	return nil
}

// DecodeTree parses a tree body, rejecting anything EncodeTree would not have
// produced: invalid modes or names, unsorted or duplicate names, non-minimal
// varints, truncation and trailing bytes. Errors wrap ErrCorrupt. Entry names
// alias body.
func DecodeTree(body []byte) (*Tree, error) {
	d := reader{buf: body}
	count, err := d.uvarint("entry count")
	if err != nil {
		return nil, fmt.Errorf("%w: tree: %w", ErrCorrupt, err)
	}
	// count is untrusted: don't preallocate more than the bytes can hold.
	t := &Tree{Entries: make([]Entry, 0, min(count, uint64(len(d.buf)/minEntrySize)))}
	for i := range count {
		var e Entry
		mode, err := d.byte("mode")
		if err != nil {
			return nil, d.errorf(i, err)
		}
		e.Mode = Mode(mode)
		if e.Name, err = d.bytes("name"); err != nil {
			return nil, d.errorf(i, err)
		}
		if e.ID, err = d.id(); err != nil {
			return nil, d.errorf(i, err)
		}
		if e.Mode != ModeTree {
			if e.Size, err = d.uvarint("size"); err != nil {
				return nil, d.errorf(i, err)
			}
		}
		if err := checkEntry(e); err != nil {
			return nil, d.errorf(i, err)
		}
		if n := len(t.Entries); n > 0 && bytes.Compare(t.Entries[n-1].Name, e.Name) >= 0 {
			return nil, d.errorf(i, fmt.Errorf("name %q not after %q", e.Name, t.Entries[n-1].Name))
		}
		t.Entries = append(t.Entries, e)
	}
	if len(d.buf) != 0 {
		return nil, fmt.Errorf("%w: tree: %d trailing bytes", ErrCorrupt, len(d.buf))
	}
	return t, nil
}

// Lookup returns the entry named name, using binary search.
func (t *Tree) Lookup(name []byte) (Entry, bool) {
	i, found := slices.BinarySearchFunc(t.Entries, name, func(e Entry, name []byte) int {
		return bytes.Compare(e.Name, name)
	})
	if !found {
		return Entry{}, false
	}
	return t.Entries[i], true
}

// reader reads canonical primitive encodings (03 §Primitive encodings).
type reader struct {
	buf []byte
}

func (d *reader) errorf(entry uint64, err error) error {
	return fmt.Errorf("%w: tree entry %d: %w", ErrCorrupt, entry, err)
}

func (d *reader) byte(what string) (byte, error) {
	if len(d.buf) < 1 {
		return 0, fmt.Errorf("truncated %s", what)
	}
	b := d.buf[0]
	d.buf = d.buf[1:]
	return b, nil
}

// uvarint reads a minimally encoded uvarint.
func (d *reader) uvarint(what string) (uint64, error) {
	v, n := binary.Uvarint(d.buf)
	switch {
	case n == 0:
		return 0, fmt.Errorf("truncated %s", what)
	case n < 0:
		return 0, fmt.Errorf("%s overflows uint64", what)
	case n != uvarintLen(v):
		return 0, fmt.Errorf("non-minimal varint for %s", what)
	}
	d.buf = d.buf[n:]
	return v, nil
}

// bytes reads uvarint(len) ‖ raw bytes. The result aliases the input.
func (d *reader) bytes(what string) ([]byte, error) {
	n, err := d.uvarint(what + " length")
	if err != nil {
		return nil, err
	}
	if n > uint64(len(d.buf)) {
		return nil, fmt.Errorf("truncated %s: length %d, %d bytes left", what, n, len(d.buf))
	}
	b := d.buf[:n:n]
	d.buf = d.buf[n:]
	return b, nil
}

func (d *reader) id() (hash.ID, error) {
	var id hash.ID
	if len(d.buf) < hash.Size {
		return id, errors.New("truncated id")
	}
	copy(id[:], d.buf)
	d.buf = d.buf[hash.Size:]
	return id, nil
}

func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}
