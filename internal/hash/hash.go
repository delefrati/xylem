// Package hash defines ObjectID (BLAKE3-256), its hex form and prefix parsing.
//
// Ids are computed as BLAKE3-256(header || body); see 03-data-model.md
// §Hashing and ADR-004. The header is built by package object.
package hash

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/zeebo/blake3"
)

// Size is the length of an ID in bytes.
const Size = 32

// HexSize is the length of an ID's hex form.
const HexSize = 2 * Size

// MinPrefixLen is the shortest hex prefix ParsePrefix accepts.
const MinPrefixLen = 8

var (
	// ErrInvalidHex reports a string that is not a valid hex id or prefix.
	ErrInvalidHex = errors.New("invalid hex id")
	// ErrPrefixTooShort reports a prefix shorter than MinPrefixLen.
	ErrPrefixTooShort = errors.New("id prefix too short")
)

// ID is a content address: the BLAKE3-256 hash of an object's header and body.
// The zero value is not the id of any object.
type ID [Size]byte

// Sum returns BLAKE3-256(header || body).
func Sum(header, body []byte) ID {
	h := blake3.New()
	_, _ = h.Write(header) // blake3.Hasher.Write never fails
	_, _ = h.Write(body)
	var id ID
	h.Sum(id[:0])
	return id
}

// String returns the 64-character lowercase hex form of id.
func (id ID) String() string {
	return hex.EncodeToString(id[:])
}

// Short returns the first n hex characters of id, with n clamped to [0, HexSize].
func (id ID) Short(n int) string {
	n = min(max(n, 0), HexSize)
	return id.String()[:n]
}

// IsZero reports whether id is the zero value.
func (id ID) IsZero() bool {
	return id == ID{}
}

// Compare returns -1, 0 or +1 depending on whether a sorts before, equal to or
// after b in bytewise order.
func Compare(a, b ID) int {
	return bytes.Compare(a[:], b[:])
}

// ParseHex parses the full 64-character hex form of an id (either case).
func ParseHex(s string) (ID, error) {
	var id ID
	if len(s) != HexSize {
		return id, fmt.Errorf("%w: %q has %d characters, want %d", ErrInvalidHex, s, len(s), HexSize)
	}
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return id, fmt.Errorf("%w: %q", ErrInvalidHex, s)
	}
	return id, nil
}

// ParsePrefix parses a hex id prefix of MinPrefixLen to HexSize characters and
// returns the byte range it covers, for queries of the form
// id >= lo AND (hi IS NULL OR id < hi).
//
// lo is inclusive. An odd-length prefix is padded with '0', so its last nibble
// covers all 16 values. hi is exclusive, and nil when the prefix is all 'f'
// (no upper bound).
func ParsePrefix(s string) (lo, hi []byte, err error) {
	if len(s) < MinPrefixLen {
		return nil, nil, fmt.Errorf("%w: %q has %d characters, want at least %d", ErrPrefixTooShort, s, len(s), MinPrefixLen)
	}
	if len(s) > HexSize {
		return nil, nil, fmt.Errorf("%w: %q has %d characters, want at most %d", ErrInvalidHex, s, len(s), HexSize)
	}
	odd := len(s)%2 == 1
	padded := s
	if odd {
		padded += "0"
	}
	lo, err = hex.DecodeString(padded)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %q", ErrInvalidHex, s)
	}

	// hi = lo + one unit of the prefix's last digit, with carry.
	hi = bytes.Clone(lo)
	step := byte(1)
	if odd {
		step = 0x10
	}
	for i := len(hi) - 1; i >= 0; i-- {
		carry := hi[i] > 0xff-step
		hi[i] += step // wraps on carry
		if !carry {
			return lo, hi, nil
		}
		step = 1
	}
	return lo, nil, nil // prefix was all 'f': no upper bound
}

// MarshalText encodes id as 64 hex characters (for JSON and config files).
func (id ID) MarshalText() ([]byte, error) {
	return []byte(id.String()), nil
}

// UnmarshalText parses 64 hex characters; empty text is the zero id.
func (id *ID) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*id = ID{}
		return nil
	}
	parsed, err := ParseHex(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}
