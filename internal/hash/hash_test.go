package hash

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Official BLAKE3 test vectors (input byte i is i % 251), default 32-byte output.
func TestSumBLAKE3Vectors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		inputLen int
		want     string
	}{
		{0, "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"},
		{1, "2d3adedff11b61f14c886e35afa036736dcd87a74d27b5c1510225d0f592e213"},
	}
	for _, tt := range tests {
		input := make([]byte, tt.inputLen)
		for i := range input {
			input[i] = byte(i % 251)
		}
		if got := Sum(nil, input).String(); got != tt.want {
			t.Errorf("Sum(len %d) = %s, want %s", tt.inputLen, got, tt.want)
		}
	}
}

func TestSumConcatenatesHeaderAndBody(t *testing.T) {
	t.Parallel()
	data := []byte("blob 5\x00hello")
	want := Sum(nil, data)
	for split := range len(data) + 1 {
		if got := Sum(data[:split], data[split:]); got != want {
			t.Errorf("Sum split at %d = %s, want %s", split, got, want)
		}
	}
	if Sum([]byte("blob 0\x00"), nil) == Sum([]byte("tree 0\x00"), nil) {
		t.Error("headers of different kinds must give different ids")
	}
}

func TestStringParseHexRoundTrip(t *testing.T) {
	t.Parallel()
	src := rand.NewChaCha8([32]byte{1})
	for range 100 {
		var id ID
		_, _ = src.Read(id[:])
		s := id.String()
		if len(s) != HexSize || strings.ToLower(s) != s {
			t.Fatalf("String() = %q, want %d lowercase hex characters", s, HexSize)
		}
		got, err := ParseHex(s)
		if err != nil || got != id {
			t.Fatalf("ParseHex(%q) = %v, %v; want %v", s, got, err, id)
		}
		if got, err := ParseHex(strings.ToUpper(s)); err != nil || got != id {
			t.Fatalf("ParseHex(upper %q) = %v, %v; want %v", s, got, err, id)
		}
	}
}

func TestParseHexErrors(t *testing.T) {
	t.Parallel()
	valid := strings.Repeat("ab", Size)
	tests := map[string]string{
		"empty":     "",
		"too short": valid[:HexSize-1],
		"too long":  valid + "a",
		"non-hex":   "g" + valid[1:],
		"space":     " " + valid[1:],
	}
	for name, in := range tests {
		if _, err := ParseHex(in); !errors.Is(err, ErrInvalidHex) {
			t.Errorf("%s: ParseHex(%q) error = %v, want ErrInvalidHex", name, in, err)
		}
	}
}

func TestShort(t *testing.T) {
	t.Parallel()
	id, err := ParseHex("0123456789abcdef" + strings.Repeat("0", HexSize-16))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		n    int
		want string
	}{
		{-1, ""},
		{0, ""},
		{8, "01234567"},
		{12, "0123456789ab"},
		{HexSize, id.String()},
		{HexSize + 10, id.String()},
	}
	for _, tt := range tests {
		if got := id.Short(tt.n); got != tt.want {
			t.Errorf("Short(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestIsZeroAndCompare(t *testing.T) {
	t.Parallel()
	var zero ID
	one := ID{31: 1}
	big := ID{0: 0xff}
	if !zero.IsZero() || one.IsZero() {
		t.Errorf("IsZero: zero=%v one=%v", zero.IsZero(), one.IsZero())
	}
	tests := []struct {
		a, b ID
		want int
	}{
		{zero, zero, 0},
		{zero, one, -1},
		{one, zero, 1},
		{one, big, -1},
		{big, big, 0},
	}
	for _, tt := range tests {
		if got := Compare(tt.a, tt.b); got != tt.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", tt.a.Short(4), tt.b.Short(4), got, tt.want)
		}
	}
}

func TestParsePrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		prefix string
		lo, hi string // hex; hi "" means nil (open)
	}{
		{"even", "0123abcd", "0123abcd", "0123abce"},
		{"even with carry", "0123abff", "0123abff", "0123ac00"},
		{"even carry through several bytes", "01ffffff", "01ffffff", "02000000"},
		{"odd pads with 0", "0123abcd1", "0123abcd10", "0123abcd20"},
		{"odd last nibble f carries", "0123abcdf", "0123abcdf0", "0123abce00"},
		{"uppercase", "0123ABCD", "0123abcd", "0123abce"},
		{"all f even is open", "ffffffff", "ffffffff", ""},
		{"all f odd is open", "fffffffff", "fffffffff0", ""},
		{"full id", strings.Repeat("0", HexSize-1) + "1", strings.Repeat("0", HexSize-1) + "1", strings.Repeat("0", HexSize-1) + "2"},
		{"full all f id is open", strings.Repeat("f", HexSize), strings.Repeat("f", HexSize), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lo, hi, err := ParsePrefix(tt.prefix)
			if err != nil {
				t.Fatalf("ParsePrefix(%q): %v", tt.prefix, err)
			}
			if diff := cmp.Diff(mustHex(t, tt.lo), lo); diff != "" {
				t.Errorf("lo mismatch (-want +got):\n%s", diff)
			}
			var wantHi []byte
			if tt.hi != "" {
				wantHi = mustHex(t, tt.hi)
			}
			if diff := cmp.Diff(wantHi, hi); diff != "" {
				t.Errorf("hi mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParsePrefixErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		prefix string
		want   error
	}{
		{"", ErrPrefixTooShort},
		{"0123abc", ErrPrefixTooShort},
		{strings.Repeat("a", HexSize+1), ErrInvalidHex},
		{"0123abcg", ErrInvalidHex},
		{"0123abcdg", ErrInvalidHex},
	}
	for _, tt := range tests {
		if _, _, err := ParsePrefix(tt.prefix); !errors.Is(err, tt.want) {
			t.Errorf("ParsePrefix(%q) error = %v, want %v", tt.prefix, err, tt.want)
		}
	}
}

// The range returned by ParsePrefix must contain exactly the ids whose hex form
// starts with the prefix.
func TestParsePrefixRangeMatchesHasPrefix(t *testing.T) {
	t.Parallel()
	src := rand.NewChaCha8([32]byte{2})
	intN := func(n int) int { // n <= 256; modulo bias is fine for test data
		var b [1]byte
		_, _ = src.Read(b[:])
		return int(b[0]) % n
	}
	inRange := func(id ID, lo, hi []byte) bool {
		return bytes.Compare(id[:], lo) >= 0 && (hi == nil || bytes.Compare(id[:], hi) < 0)
	}
	randomID := func() ID {
		var id ID
		_, _ = src.Read(id[:])
		// Bias towards 0xff and 0xf? bytes to exercise carries and the open bound.
		for i := range id {
			switch intN(4) {
			case 0:
				id[i] = 0xff
			case 1:
				id[i] |= 0xf0
			}
		}
		return id
	}
	for range 2000 {
		target := randomID()
		n := MinPrefixLen + intN(HexSize-MinPrefixLen+1)
		prefix := target.String()[:n]
		lo, hi, err := ParsePrefix(prefix)
		if err != nil {
			t.Fatalf("ParsePrefix(%q): %v", prefix, err)
		}
		if !inRange(target, lo, hi) {
			t.Fatalf("id %s not in range of its own prefix %q", target, prefix)
		}
		// Ids near the target: same prefix iff in range.
		for range 20 {
			other := target
			var flip [1]byte
			for flip[0] == 0 {
				_, _ = src.Read(flip[:])
			}
			other[intN(Size)] ^= flip[0]
			want := strings.HasPrefix(other.String(), prefix)
			if got := inRange(other, lo, hi); got != want {
				t.Fatalf("prefix %q, id %s: inRange = %v, HasPrefix = %v", prefix, other, got, want)
			}
		}
	}
}

func TestTextRoundTrip(t *testing.T) {
	t.Parallel()
	id := Sum(nil, []byte("x"))
	text, err := id.MarshalText()
	if err != nil || string(text) != id.String() {
		t.Fatalf("MarshalText = %s, %v", text, err)
	}
	var got ID
	if err := got.UnmarshalText(text); err != nil || got != id {
		t.Errorf("UnmarshalText = %v, %v", got, err)
	}
	if err := got.UnmarshalText(nil); err != nil || !got.IsZero() {
		t.Errorf("empty text: %v, %v", got, err)
	}
	if err := got.UnmarshalText([]byte("zz")); err == nil {
		t.Error("bad text accepted")
	}
}
