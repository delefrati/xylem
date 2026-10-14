// Package changeset defines the changeset model, its canonical encoding and
// shape validation (03-data-model.md §Changeset and §Validation rules).
//
// A changeset records its parents, metadata and the files changed relative to
// its first parent. Its id is the object id of its canonical body
// (kind "changeset"); the root tree is derived data and not part of it
// (ADR-006).
package changeset

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/delefrati/xylem/internal/canon"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
)

// FormatVersion is the first field of every changeset body.
const FormatVersion = 1

// Limits from 03 §Validation rules 1 and 5.
const (
	MaxParents       = 16
	MaxMessageSize   = 1 << 20
	MaxExtraValue    = 64 << 10
	MaxChanges       = 1_000_000
	MaxSummaryLength = 200 // runes
)

// Standard extra keys (03 §Changeset). Unknown keys are preserved.
const (
	ExtraGitSHA             = "git-sha"
	ExtraRebasedFrom        = "rebased-from"
	ExtraGitParentsUnmapped = "git-extra-parents-unmapped"
	ExtraGitEncoding        = "git-encoding"
)

// ErrInvalid reports a changeset that cannot be encoded or breaks a shape rule.
var ErrInvalid = errors.New("invalid changeset")

// Op is a change operation.
type Op uint8

// Change operations. The zero Op is invalid.
const (
	OpPut    Op = 0x01
	OpDelete Op = 0x02
)

func (o Op) String() string {
	switch o {
	case OpPut:
		return "PUT"
	case OpDelete:
		return "DELETE"
	}
	return fmt.Sprintf("Op(%d)", uint8(o))
}

// Signature identifies an author or committer.
type Signature struct {
	NameEmail []byte // "Name <email>", raw bytes (may be non-UTF-8)
	Time      int64  // Unix seconds
	TZMinutes int32  // signed UTC offset, e.g. -180 for -0300
}

// Change is one path changed relative to the first parent. Mode, Size and
// Blob are set for OpPut and must be zero for OpDelete.
type Change struct {
	Path []byte
	Op   Op
	Mode object.Mode // ModeFile, ModeExec or ModeSymlink
	Size uint64
	Blob hash.ID
}

// Changeset mirrors the canonical changeset body. Changes and Extras may be in
// any order; Encode canonicalizes copies of them.
type Changeset struct {
	Parents   []hash.ID // [0] is the first parent
	Author    Signature
	Committer Signature
	Message   []byte
	Extras    map[string][]byte
	Changes   []Change
}

// ShapeOptions relaxes ValidateShape for trusted callers.
type ShapeOptions struct {
	// AllowEmpty permits a single-parent changeset with no changes, for
	// importing empty git commits (08-git-import.md).
	AllowEmpty bool
}

// Encode returns the canonical body:
//
//	uvarint(1) ‖ uvarint(parent_count) ‖ id* ‖
//	bytes(author) ‖ varint(time) ‖ varint(tz) ‖ bytes(committer) ‖ varint(time) ‖ varint(tz) ‖
//	bytes(message) ‖ uvarint(extra_count) ‖ (bytes(key) ‖ bytes(value))* ‖
//	uvarint(change_count) ‖ change*
//	change = bytes(path) ‖ u8(op) ‖ [u8(mode) ‖ uvarint(size) ‖ id(blob) if PUT]
//
// Extras are sorted by key and changes by path; c is not modified. Encode fails,
// wrapping ErrInvalid, if two changes share a path or a change's op and mode
// are inconsistent. Other shape rules are checked by ValidateShape.
func (c *Changeset) Encode() ([]byte, error) {
	changes, err := c.sortedChanges()
	if err != nil {
		return nil, err
	}
	keys := slices.Sorted(maps.Keys(c.Extras))

	size := 64 + len(c.Parents)*hash.Size + len(c.Author.NameEmail) + len(c.Committer.NameEmail) + len(c.Message)
	for _, k := range keys {
		size += 2*binary.MaxVarintLen64 + len(k) + len(c.Extras[k])
	}
	for _, ch := range changes {
		size += 2*binary.MaxVarintLen64 + 2 + len(ch.Path) + hash.Size
	}

	b := make([]byte, 0, size)
	b = binary.AppendUvarint(b, FormatVersion)
	b = binary.AppendUvarint(b, uint64(len(c.Parents)))
	for _, p := range c.Parents {
		b = append(b, p[:]...)
	}
	b = appendSignature(b, c.Author)
	b = appendSignature(b, c.Committer)
	b = canon.AppendBytes(b, c.Message)
	b = binary.AppendUvarint(b, uint64(len(keys)))
	for _, k := range keys {
		b = canon.AppendBytes(b, []byte(k))
		b = canon.AppendBytes(b, c.Extras[k])
	}
	b = binary.AppendUvarint(b, uint64(len(changes)))
	for _, ch := range changes {
		b = canon.AppendBytes(b, ch.Path)
		b = append(b, byte(ch.Op))
		if ch.Op == OpPut {
			b = append(b, byte(ch.Mode))
			b = binary.AppendUvarint(b, ch.Size)
			b = append(b, ch.Blob[:]...)
		}
	}
	return b, nil
}

// ID returns the changeset's id: the object id of its canonical body.
func (c *Changeset) ID() (hash.ID, error) {
	body, err := c.Encode()
	if err != nil {
		return hash.ID{}, err
	}
	return object.ID(object.KindChangeset, body), nil
}

// sortedChanges returns a copy of c.Changes sorted by path, checking each
// change's op and mode and that paths are unique.
func (c *Changeset) sortedChanges() ([]Change, error) {
	changes := slices.Clone(c.Changes)
	slices.SortFunc(changes, func(a, b Change) int { return bytes.Compare(a.Path, b.Path) })
	for i, ch := range changes {
		if err := checkChange(ch); err != nil {
			return nil, fmt.Errorf("%w: change %q: %w", ErrInvalid, ch.Path, err)
		}
		if i > 0 && bytes.Equal(changes[i-1].Path, ch.Path) {
			return nil, fmt.Errorf("%w: duplicate change for path %q", ErrInvalid, ch.Path)
		}
	}
	return changes, nil
}

func appendSignature(b []byte, s Signature) []byte {
	b = canon.AppendBytes(b, s.NameEmail)
	b = binary.AppendVarint(b, s.Time)
	return binary.AppendVarint(b, int64(s.TZMinutes))
}

// checkChange checks op/mode consistency.
func checkChange(ch Change) error {
	switch ch.Op {
	case OpPut:
		if ch.Mode != object.ModeFile && ch.Mode != object.ModeExec && ch.Mode != object.ModeSymlink {
			return fmt.Errorf("PUT with mode %v", ch.Mode)
		}
	case OpDelete:
		if ch.Mode != 0 || ch.Size != 0 || !ch.Blob.IsZero() {
			return errors.New("DELETE with mode, size or blob set")
		}
	default:
		return fmt.Errorf("invalid op %d", uint8(ch.Op))
	}
	return nil
}

// ValidateShape checks the rules that need no repository state: at most
// MaxParents parents (rule 1); valid, unique change paths and at least one
// change unless the changeset is a merge, a root, or opts.AllowEmpty is set
// (rule 2); the size limits of rule 5; op/mode consistency; and extra keys that
// are non-empty UTF-8 (the proto field is map<string, bytes>). Errors wrap
// ErrInvalid. Rules 1, 3 and 4 that need the store or parent trees are checked
// by package repo.
func (c *Changeset) ValidateShape(opts ShapeOptions) error {
	if len(c.Parents) > MaxParents {
		return fmt.Errorf("%w: %d parents, max %d", ErrInvalid, len(c.Parents), MaxParents)
	}
	if len(c.Changes) == 0 && len(c.Parents) == 1 && !opts.AllowEmpty {
		return fmt.Errorf("%w: no changes", ErrInvalid)
	}
	if len(c.Changes) > MaxChanges {
		return fmt.Errorf("%w: %d changes, max %d", ErrInvalid, len(c.Changes), MaxChanges)
	}
	if len(c.Message) > MaxMessageSize {
		return fmt.Errorf("%w: message is %d bytes, max %d", ErrInvalid, len(c.Message), MaxMessageSize)
	}
	for k, v := range c.Extras {
		if k == "" || !utf8.ValidString(k) {
			return fmt.Errorf("%w: extra key %q must be non-empty UTF-8", ErrInvalid, k)
		}
		if len(v) > MaxExtraValue {
			return fmt.Errorf("%w: extra %q is %d bytes, max %d", ErrInvalid, k, len(v), MaxExtraValue)
		}
	}
	for _, ch := range c.Changes {
		if err := paths.ValidPath(ch.Path); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	_, err := c.sortedChanges()
	return err
}

// Summary returns the first line of the message as valid UTF-8 without NUL
// bytes (it is stored in a Postgres TEXT column), truncated to
// MaxSummaryLength runes. A trailing '\r' is dropped.
func (c *Changeset) Summary() string {
	line, _, _ := bytes.Cut(c.Message, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	s := strings.ToValidUTF8(string(line), "�")
	s = strings.ReplaceAll(s, "\x00", "�")
	if utf8.RuneCountInString(s) <= MaxSummaryLength {
		return s
	}
	n := 0
	for i := range s {
		if n == MaxSummaryLength {
			return s[:i]
		}
		n++
	}
	return s
}

// minChangeSize is the smallest encoded change: empty path length and op.
const minChangeSize = 2

// Decode parses a canonical changeset body, rejecting anything Encode would not
// have produced: an unknown format version, non-minimal varints, out-of-range
// time zones, unsorted or duplicate extras or changes, inconsistent op and
// mode, truncation and trailing bytes. Errors wrap object.ErrCorrupt. Byte
// fields alias body. Decode does not apply ValidateShape.
func Decode(body []byte) (*Changeset, error) {
	c, err := decode(canon.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: changeset: %w", object.ErrCorrupt, err)
	}
	return c, nil
}

func decode(r *canon.Reader) (*Changeset, error) {
	version, err := r.Uvarint()
	if err != nil {
		return nil, fmt.Errorf("version: %w", err)
	}
	if version != FormatVersion {
		return nil, fmt.Errorf("unknown format version %d", version)
	}

	c := &Changeset{}
	nParents, err := r.Uvarint()
	if err != nil {
		return nil, fmt.Errorf("parent count: %w", err)
	}
	if nParents > 0 {
		c.Parents = make([]hash.ID, 0, r.CapHint(nParents, hash.Size))
	}
	for i := range nParents {
		id, err := r.ID()
		if err != nil {
			return nil, fmt.Errorf("parent %d: %w", i, err)
		}
		c.Parents = append(c.Parents, id)
	}

	if c.Author, err = decodeSignature(r); err != nil {
		return nil, fmt.Errorf("author: %w", err)
	}
	if c.Committer, err = decodeSignature(r); err != nil {
		return nil, fmt.Errorf("committer: %w", err)
	}
	if c.Message, err = r.Bytes(); err != nil {
		return nil, fmt.Errorf("message: %w", err)
	}

	nExtras, err := r.Uvarint()
	if err != nil {
		return nil, fmt.Errorf("extra count: %w", err)
	}
	var prevKey []byte
	for i := range nExtras {
		key, err := r.Bytes()
		if err != nil {
			return nil, fmt.Errorf("extra %d key: %w", i, err)
		}
		value, err := r.Bytes()
		if err != nil {
			return nil, fmt.Errorf("extra %q value: %w", key, err)
		}
		if i > 0 && bytes.Compare(prevKey, key) >= 0 {
			return nil, fmt.Errorf("extra key %q not after %q", key, prevKey)
		}
		if c.Extras == nil {
			c.Extras = make(map[string][]byte)
		}
		c.Extras[string(key)] = value
		prevKey = key
	}

	nChanges, err := r.Uvarint()
	if err != nil {
		return nil, fmt.Errorf("change count: %w", err)
	}
	if nChanges > 0 {
		c.Changes = make([]Change, 0, r.CapHint(nChanges, minChangeSize))
	}
	for i := range nChanges {
		ch, err := decodeChange(r)
		if err != nil {
			return nil, fmt.Errorf("change %d: %w", i, err)
		}
		if n := len(c.Changes); n > 0 && bytes.Compare(c.Changes[n-1].Path, ch.Path) >= 0 {
			return nil, fmt.Errorf("change path %q not after %q", ch.Path, c.Changes[n-1].Path)
		}
		c.Changes = append(c.Changes, ch)
	}

	if r.Len() != 0 {
		return nil, fmt.Errorf("%d trailing bytes", r.Len())
	}
	return c, nil
}

func decodeSignature(r *canon.Reader) (Signature, error) {
	var s Signature
	var err error
	if s.NameEmail, err = r.Bytes(); err != nil {
		return s, fmt.Errorf("name: %w", err)
	}
	if s.Time, err = r.Varint(); err != nil {
		return s, fmt.Errorf("time: %w", err)
	}
	tz, err := r.Varint()
	if err != nil {
		return s, fmt.Errorf("tz: %w", err)
	}
	if tz < math.MinInt32 || tz > math.MaxInt32 {
		return s, fmt.Errorf("tz %d out of range", tz)
	}
	s.TZMinutes = int32(tz)
	return s, nil
}

func decodeChange(r *canon.Reader) (Change, error) {
	var ch Change
	var err error
	if ch.Path, err = r.Bytes(); err != nil {
		return ch, fmt.Errorf("path: %w", err)
	}
	op, err := r.Byte()
	if err != nil {
		return ch, fmt.Errorf("op: %w", err)
	}
	ch.Op = Op(op)
	if ch.Op == OpPut {
		mode, err := r.Byte()
		if err != nil {
			return ch, fmt.Errorf("mode: %w", err)
		}
		ch.Mode = object.Mode(mode)
		if ch.Size, err = r.Uvarint(); err != nil {
			return ch, fmt.Errorf("size: %w", err)
		}
		if ch.Blob, err = r.ID(); err != nil {
			return ch, fmt.Errorf("blob: %w", err)
		}
	}
	if err := checkChange(ch); err != nil {
		return ch, err
	}
	return ch, nil
}
