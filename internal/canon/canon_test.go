package canon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/delefrati/xylem/internal/hash"
)

func TestUvarint(t *testing.T) {
	t.Parallel()
	for _, v := range []uint64{0, 1, 127, 128, 300, 1 << 32, math.MaxUint64} {
		r := NewReader(binary.AppendUvarint(nil, v))
		got, err := r.Uvarint()
		if err != nil || got != v || r.Len() != 0 {
			t.Errorf("Uvarint(%d) = %d, %v, %d left", v, got, err, r.Len())
		}
	}
	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty", nil, ErrTruncated},
		{"unterminated", []byte{0x80}, ErrTruncated},
		{"non-minimal zero", []byte{0x80, 0x00}, ErrNonMinimal},
		{"non-minimal one", []byte{0x81, 0x00}, ErrNonMinimal},
		{"non-minimal long", []byte{0xac, 0x82, 0x00}, ErrNonMinimal},
		{"overflow", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02}, ErrOverflow},
		{"too long", bytes.Repeat([]byte{0x80}, 11), ErrOverflow},
	}
	for _, tt := range tests {
		if _, err := NewReader(tt.in).Uvarint(); !errors.Is(err, tt.want) {
			t.Errorf("%s: Uvarint error = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestVarint(t *testing.T) {
	t.Parallel()
	for _, v := range []int64{0, 1, -1, 63, -64, 64, -65, -180, 330, math.MaxInt64, math.MinInt64} {
		r := NewReader(binary.AppendVarint(nil, v))
		got, err := r.Varint()
		if err != nil || got != v || r.Len() != 0 {
			t.Errorf("Varint(%d) = %d, %v, %d left", v, got, err, r.Len())
		}
	}
	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty", nil, ErrTruncated},
		{"non-minimal", []byte{0x82, 0x00}, ErrNonMinimal},
		{"overflow", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02}, ErrOverflow},
	}
	for _, tt := range tests {
		if _, err := NewReader(tt.in).Varint(); !errors.Is(err, tt.want) {
			t.Errorf("%s: Varint error = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestBytes(t *testing.T) {
	t.Parallel()
	in := AppendBytes(AppendBytes(nil, []byte("abc")), nil)
	if !bytes.Equal(in, []byte{3, 'a', 'b', 'c', 0}) {
		t.Fatalf("AppendBytes = %x", in)
	}
	r := NewReader(in)
	got, err := r.Bytes()
	if err != nil || string(got) != "abc" {
		t.Fatalf("Bytes = %q, %v", got, err)
	}
	if cap(got) != 3 {
		t.Errorf("Bytes capacity = %d, want clipped to 3", cap(got))
	}
	if got, err := r.Bytes(); err != nil || len(got) != 0 || r.Len() != 0 {
		t.Errorf("empty Bytes = %q, %v, %d left", got, err, r.Len())
	}
	for name, in := range map[string][]byte{
		"no length":          nil,
		"length past end":    {5, 'a'},
		"non-minimal length": {0x81, 0x00, 'a'},
	} {
		if _, err := NewReader(in).Bytes(); err == nil {
			t.Errorf("%s: Bytes accepted %x", name, in)
		}
	}
}

func TestByteAndID(t *testing.T) {
	t.Parallel()
	var id hash.ID
	for i := range id {
		id[i] = byte(i)
	}
	r := NewReader(append([]byte{7}, id[:]...))
	if b, err := r.Byte(); err != nil || b != 7 {
		t.Errorf("Byte = %d, %v", b, err)
	}
	if got, err := r.ID(); err != nil || got != id {
		t.Errorf("ID = %v, %v", got, err)
	}
	if _, err := r.Byte(); !errors.Is(err, ErrTruncated) {
		t.Errorf("Byte at end: %v", err)
	}
	if _, err := NewReader(id[:31]).ID(); !errors.Is(err, ErrTruncated) {
		t.Errorf("short ID: %v", err)
	}
}

func TestCapHint(t *testing.T) {
	t.Parallel()
	r := NewReader(make([]byte, 100))
	tests := []struct {
		count   uint64
		minSize int
		want    int
	}{
		{0, 10, 0},
		{3, 10, 3},
		{10, 10, 10},
		{11, 10, 10},
		{math.MaxUint64, 10, 10},
		{math.MaxInt32, 1, 100},
		{5, 33, 3},
	}
	for _, tt := range tests {
		if got := r.CapHint(tt.count, tt.minSize); got != tt.want {
			t.Errorf("CapHint(%d, %d) = %d, want %d", tt.count, tt.minSize, got, tt.want)
		}
	}
}
