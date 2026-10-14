// Package object implements the stored object format: a zstd frame of
// header || body, where header is "<kind> <decimal body length>\x00" and the
// object id is BLAKE3-256(header || body). The same bytes are used at rest and
// on the wire (ADR-005). See 03-data-model.md §Hashing and §Stored object
// format.
package object

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/delefrati/xylem/internal/hash"
)

// Kind is the type of an object, recorded in its header.
type Kind uint8

// Object kinds. The zero Kind is invalid.
const (
	KindBlob Kind = iota + 1
	KindTree
	KindChangeset
	KindDerived
)

var kindNames = [...]string{
	KindBlob:      "blob",
	KindTree:      "tree",
	KindChangeset: "changeset",
	KindDerived:   "derived",
}

// Valid reports whether k is one of the defined kinds.
func (k Kind) Valid() bool {
	return k >= KindBlob && k <= KindDerived
}

// String returns the header name of k ("blob", "tree", ...).
func (k Kind) String() string {
	if !k.Valid() {
		return "Kind(" + strconv.Itoa(int(k)) + ")"
	}
	return kindNames[k]
}

func parseKind(s []byte) (Kind, bool) {
	for k := KindBlob; k <= KindDerived; k++ {
		if string(s) == kindNames[k] {
			return k, true
		}
	}
	return 0, false
}

// MaxDecodedSize is the largest decompressed object (header plus body) that
// Decode accepts: a 64 MiB body plus room for the header. It guards against
// decompression bombs. Blob size policy is enforced by writers.
const MaxDecodedSize = 64<<20 + 1<<10

// zstdLevel is the compression level for stored objects (03 §Stored object format).
const zstdLevel = 3

var (
	// ErrCorrupt reports stored bytes that are not a valid object.
	ErrCorrupt = errors.New("corrupt object")
	// ErrTooLarge reports an object whose decompressed size exceeds MaxDecodedSize.
	ErrTooLarge = errors.New("object too large")
	// ErrHashMismatch reports an object whose content does not hash to the expected id.
	ErrHashMismatch = errors.New("object hash mismatch")
)

// The encoder and decoder are safe for concurrent EncodeAll/DecodeAll calls and
// keep internal pools of per-call state sized to GOMAXPROCS.
var (
	encoder = sync.OnceValue(func() *zstd.Encoder {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(zstdLevel)))
		if err != nil {
			panic(fmt.Sprintf("object: creating zstd encoder: %v", err))
		}
		return enc
	})
	decoder = sync.OnceValue(func() *zstd.Decoder {
		dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(MaxDecodedSize))
		if err != nil {
			panic(fmt.Sprintf("object: creating zstd decoder: %v", err))
		}
		return dec
	})
)

// Header returns "<kind> <n>\x00". It panics if kind is invalid or n is negative.
func Header(kind Kind, n int) []byte {
	return appendHeader(nil, kind, n)
}

func appendHeader(dst []byte, kind Kind, n int) []byte {
	if !kind.Valid() || n < 0 {
		panic(fmt.Sprintf("object: invalid header %v %d", kind, n))
	}
	dst = append(dst, kindNames[kind]...)
	dst = append(dst, ' ')
	dst = strconv.AppendInt(dst, int64(n), 10)
	return append(dst, 0)
}

// ID returns the id of an object without encoding it.
func ID(kind Kind, body []byte) hash.ID {
	return hash.Sum(Header(kind, len(body)), body)
}

// Encode returns the id of the object and its stored form. It panics if kind is
// invalid. Encode does not enforce MaxDecodedSize.
func Encode(kind Kind, body []byte) (hash.ID, []byte) {
	raw := appendHeader(make([]byte, 0, 32+len(body)), kind, len(body))
	headerLen := len(raw)
	raw = append(raw, body...)
	id := hash.Sum(raw[:headerLen], raw[headerLen:])
	return id, encoder().EncodeAll(raw, nil)
}

// Decode decompresses stored and splits it into kind and body. It does not
// check the id; use Verify or DecodeVerify for bytes from an untrusted tier.
func Decode(stored []byte) (Kind, []byte, error) {
	raw, err := decompress(stored)
	if err != nil {
		return 0, nil, err
	}
	return parse(raw)
}

// DecodeVerify is Decode plus a check that the object's content hashes to id.
func DecodeVerify(id hash.ID, stored []byte) (Kind, []byte, error) {
	raw, err := decompress(stored)
	if err != nil {
		return 0, nil, err
	}
	if got := hash.Sum(nil, raw); got != id {
		return 0, nil, fmt.Errorf("%w: want %s, got %s", ErrHashMismatch, id, got)
	}
	return parse(raw)
}

// Verify checks that stored is a valid object whose id is id.
func Verify(id hash.ID, stored []byte) error {
	_, _, err := DecodeVerify(id, stored)
	return err
}

func decompress(stored []byte) ([]byte, error) {
	raw, err := decoder().DecodeAll(stored, nil)
	switch {
	case errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded):
		return nil, fmt.Errorf("%w: exceeds %d bytes", ErrTooLarge, MaxDecodedSize)
	case err != nil:
		return nil, fmt.Errorf("%w: zstd: %w", ErrCorrupt, err)
	}
	return raw, nil
}

// parse splits raw into kind and body, accepting only a canonical header whose
// length matches the body exactly.
func parse(raw []byte) (Kind, []byte, error) {
	header, body, ok := bytes.Cut(raw, []byte{0})
	if !ok {
		return 0, nil, fmt.Errorf("%w: header has no NUL terminator", ErrCorrupt)
	}
	kindName, lenStr, ok := bytes.Cut(header, []byte{' '})
	if !ok {
		return 0, nil, fmt.Errorf("%w: header %q has no space", ErrCorrupt, header)
	}
	kind, ok := parseKind(kindName)
	if !ok {
		return 0, nil, fmt.Errorf("%w: unknown kind %q", ErrCorrupt, kindName)
	}
	n, err := strconv.Atoi(string(lenStr))
	if err != nil || n < 0 || strconv.Itoa(n) != string(lenStr) {
		return 0, nil, fmt.Errorf("%w: non-canonical length %q", ErrCorrupt, lenStr)
	}
	if n != len(body) {
		return 0, nil, fmt.Errorf("%w: header says %d bytes, body has %d", ErrCorrupt, n, len(body))
	}
	return kind, body, nil
}
