// Package canon reads and writes the primitive encodings shared by tree and
// changeset bodies (03-data-model.md §Primitive encodings), accepting only
// canonical input: varints must be minimally encoded.
package canon

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/delefrati/xylem/internal/hash"
)

var (
	// ErrTruncated reports input that ends inside a value.
	ErrTruncated = errors.New("truncated")
	// ErrNonMinimal reports a varint with redundant continuation bytes.
	ErrNonMinimal = errors.New("non-minimal varint")
	// ErrOverflow reports a varint that does not fit in 64 bits.
	ErrOverflow = errors.New("varint overflows 64 bits")
)

// AppendBytes appends uvarint(len(b)) ‖ b.
func AppendBytes(dst, b []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// Reader consumes canonical primitive encodings from a byte slice.
type Reader struct {
	buf []byte
}

// NewReader returns a Reader over b. Values returned by Bytes alias b.
func NewReader(b []byte) *Reader {
	return &Reader{buf: b}
}

// Len returns the number of unread bytes.
func (r *Reader) Len() int {
	return len(r.buf)
}

// CapHint returns a safe preallocation size for count items of at least
// minSize bytes each: count, capped by how many such items the unread bytes
// could hold. count comes from untrusted input.
func (r *Reader) CapHint(count uint64, minSize int) int {
	fit := len(r.buf) / minSize
	if count >= math.MaxInt32 {
		return fit
	}
	return min(int(count), fit)
}

// Byte reads a u8.
func (r *Reader) Byte() (byte, error) {
	if len(r.buf) < 1 {
		return 0, ErrTruncated
	}
	b := r.buf[0]
	r.buf = r.buf[1:]
	return b, nil
}

// Uvarint reads a minimally encoded unsigned LEB128 varint.
func (r *Reader) Uvarint() (uint64, error) {
	v, n := binary.Uvarint(r.buf)
	if err := checkVarint(n); err != nil {
		return 0, err
	}
	var tmp [binary.MaxVarintLen64]byte
	if binary.PutUvarint(tmp[:], v) != n {
		return 0, ErrNonMinimal
	}
	r.buf = r.buf[n:]
	return v, nil
}

// Varint reads a minimally encoded zig-zag varint.
func (r *Reader) Varint() (int64, error) {
	v, n := binary.Varint(r.buf)
	if err := checkVarint(n); err != nil {
		return 0, err
	}
	var tmp [binary.MaxVarintLen64]byte
	if binary.PutVarint(tmp[:], v) != n {
		return 0, ErrNonMinimal
	}
	r.buf = r.buf[n:]
	return v, nil
}

func checkVarint(n int) error {
	switch {
	case n == 0:
		return ErrTruncated
	case n < 0:
		return ErrOverflow
	}
	return nil
}

// Bytes reads uvarint(len) ‖ raw bytes. The result aliases the input and has
// its capacity clipped, so appending to it never overwrites later input.
func (r *Reader) Bytes() ([]byte, error) {
	n, err := r.Uvarint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(r.buf)) {
		return nil, ErrTruncated
	}
	b := r.buf[:n:n]
	r.buf = r.buf[n:]
	return b, nil
}

// ID reads 32 raw bytes.
func (r *Reader) ID() (hash.ID, error) {
	var id hash.ID
	if len(r.buf) < hash.Size {
		return id, ErrTruncated
	}
	copy(id[:], r.buf)
	r.buf = r.buf[hash.Size:]
	return id, nil
}
