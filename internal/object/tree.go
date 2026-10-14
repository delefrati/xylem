package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/delefrati/xylem/internal/canon"
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
		b = canon.AppendBytes(b, e.Name)
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
	r := canon.NewReader(body)
	count, err := r.Uvarint()
	if err != nil {
		return nil, fmt.Errorf("%w: tree entry count: %w", ErrCorrupt, err)
	}
	// count is untrusted: don't preallocate more than the bytes can hold.
	t := &Tree{Entries: make([]Entry, 0, r.CapHint(count, minEntrySize))}
	for i := range count {
		e, err := decodeEntry(r)
		if err == nil {
			err = checkEntry(e)
		}
		if err == nil {
			if n := len(t.Entries); n > 0 && bytes.Compare(t.Entries[n-1].Name, e.Name) >= 0 {
				err = fmt.Errorf("name %q not after %q", e.Name, t.Entries[n-1].Name)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%w: tree entry %d: %w", ErrCorrupt, i, err)
		}
		t.Entries = append(t.Entries, e)
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("%w: tree: %d trailing bytes", ErrCorrupt, r.Len())
	}
	return t, nil
}

func decodeEntry(r *canon.Reader) (Entry, error) {
	var e Entry
	mode, err := r.Byte()
	if err != nil {
		return e, fmt.Errorf("mode: %w", err)
	}
	e.Mode = Mode(mode)
	if e.Name, err = r.Bytes(); err != nil {
		return e, fmt.Errorf("name: %w", err)
	}
	if e.ID, err = r.ID(); err != nil {
		return e, fmt.Errorf("id: %w", err)
	}
	if e.Mode != ModeTree {
		if e.Size, err = r.Uvarint(); err != nil {
			return e, fmt.Errorf("size: %w", err)
		}
	}
	return e, nil
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
