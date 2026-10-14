package object

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/klauspost/compress/zstd"

	"github.com/delefrati/xylem/internal/hash"
)

var allKinds = []Kind{KindBlob, KindTree, KindChangeset, KindDerived}

// compressRaw frames arbitrary bytes, bypassing Encode's header construction.
func compressRaw(raw []byte) []byte {
	return encoder().EncodeAll(raw, nil)
}

func TestKind(t *testing.T) {
	t.Parallel()
	want := map[Kind]string{KindBlob: "blob", KindTree: "tree", KindChangeset: "changeset", KindDerived: "derived"}
	for k, name := range want {
		if !k.Valid() || k.String() != name {
			t.Errorf("Kind %d: Valid=%v String=%q, want true %q", k, k.Valid(), k.String(), name)
		}
	}
	for _, k := range []Kind{0, KindDerived + 1, 255} {
		if k.Valid() || k.String() != fmt.Sprintf("Kind(%d)", k) {
			t.Errorf("Kind %d: Valid=%v String=%q", k, k.Valid(), k.String())
		}
	}
}

func TestHeader(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind Kind
		n    int
		want string
	}{
		{KindBlob, 0, "blob 0\x00"},
		{KindBlob, 5, "blob 5\x00"},
		{KindTree, 123, "tree 123\x00"},
		{KindChangeset, 1 << 20, "changeset 1048576\x00"},
		{KindDerived, 10, "derived 10\x00"},
	}
	for _, tt := range tests {
		if got := Header(tt.kind, tt.n); string(got) != tt.want {
			t.Errorf("Header(%v, %d) = %q, want %q", tt.kind, tt.n, got, tt.want)
		}
	}
}

func TestHeaderPanics(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		kind Kind
		n    int
	}{{0, 1}, {KindDerived + 1, 1}, {KindBlob, -1}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Header(%v, %d) did not panic", tt.kind, tt.n)
				}
			}()
			Header(tt.kind, tt.n)
		}()
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	random := make([]byte, 1<<20)
	_, _ = rand.NewChaCha8([32]byte{1}).Read(random)
	bodies := map[string][]byte{
		"empty":               {},
		"hello":               []byte("hello\n"),
		"binary":              {0, 1, 2, 0, 0xff, '\n', 0},
		"looks like a header": []byte("blob 3\x00abc"),
		"1 MiB random":        random,
		"1 MiB zeros":         make([]byte, 1<<20),
	}
	for _, kind := range allKinds {
		for name, body := range bodies {
			id, stored := Encode(kind, body)
			if want := hash.Sum(Header(kind, len(body)), body); id != want {
				t.Errorf("%v %s: Encode id = %s, want %s", kind, name, id, want)
			}
			if got := ID(kind, body); got != id {
				t.Errorf("%v %s: ID = %s, Encode id = %s", kind, name, got, id)
			}
			gotKind, gotBody, err := Decode(stored)
			if err != nil {
				t.Fatalf("%v %s: Decode: %v", kind, name, err)
			}
			if gotKind != kind || !bytes.Equal(gotBody, body) {
				t.Errorf("%v %s: Decode = %v, %d bytes; want %v, %d bytes", kind, name, gotKind, len(gotBody), kind, len(body))
			}
			if err := Verify(id, stored); err != nil {
				t.Errorf("%v %s: Verify: %v", kind, name, err)
			}
		}
	}
}

func TestEncodeDoesNotAliasBody(t *testing.T) {
	t.Parallel()
	body := []byte("abc")
	_, stored := Encode(KindBlob, body)
	body[0] = 'z'
	if _, got, _ := Decode(stored); string(got) != "abc" {
		t.Errorf("stored object changed with its input: %q", got)
	}
}

func TestEmptyBlobID(t *testing.T) {
	t.Parallel()
	// 03 §Hashing: an empty blob is BLAKE3("blob 0\x00").
	if got, want := ID(KindBlob, nil), hash.Sum([]byte("blob 0\x00"), nil); got != want {
		t.Errorf("empty blob id = %s, want %s", got, want)
	}
	if ID(KindBlob, nil) == ID(KindTree, nil) {
		t.Error("empty blob and empty tree must have different ids")
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	t.Parallel()
	valid := compressRaw([]byte("blob 3\x00abc"))
	tests := map[string][]byte{
		"empty input":      nil,
		"not zstd":         []byte("blob 3\x00abc"),
		"truncated frame":  valid[:len(valid)-3],
		"empty content":    compressRaw(nil),
		"no NUL":           compressRaw([]byte("blob 3 abc")),
		"no space":         compressRaw([]byte("blob3\x00abc")),
		"unknown kind":     compressRaw([]byte("file 3\x00abc")),
		"kind wrong case":  compressRaw([]byte("Blob 3\x00abc")),
		"empty kind":       compressRaw([]byte(" 3\x00abc")),
		"double space":     compressRaw([]byte("blob  3\x00abc")),
		"leading zero":     compressRaw([]byte("blob 03\x00abc")),
		"plus sign":        compressRaw([]byte("blob +3\x00abc")),
		"negative":         compressRaw([]byte("blob -3\x00abc")),
		"empty length":     compressRaw([]byte("blob \x00")),
		"hex length":       compressRaw([]byte("blob 0x3\x00abc")),
		"trailing space":   compressRaw([]byte("blob 3 \x00abc")),
		"body too short":   compressRaw([]byte("blob 4\x00abc")),
		"body too long":    compressRaw([]byte("blob 2\x00abc")),
		"length overflows": compressRaw([]byte("blob 99999999999999999999\x00abc")),
		"two frames":       append(bytes.Clone(valid), valid...),
		"trailing garbage": append(bytes.Clone(valid), 0, 1, 2),
	}
	for name, stored := range tests {
		if kind, body, err := Decode(stored); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: Decode = %v, %q, %v; want ErrCorrupt", name, kind, body, err)
		}
	}
}

func TestDecodeSizeLimit(t *testing.T) {
	t.Parallel()
	header := len(Header(KindBlob, MaxDecodedSize))

	// Exactly at the limit decodes.
	_, atLimit := Encode(KindBlob, make([]byte, MaxDecodedSize-header))
	if _, _, err := Decode(atLimit); err != nil {
		t.Errorf("object of exactly MaxDecodedSize: %v", err)
	}

	// One byte over is rejected (frame declares its content size).
	_, over := Encode(KindBlob, make([]byte, MaxDecodedSize-header+1))
	if _, _, err := Decode(over); !errors.Is(err, ErrTooLarge) {
		t.Errorf("object over MaxDecodedSize: err = %v, want ErrTooLarge", err)
	}

	// A streamed frame with no declared content size is still bounded.
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(Header(KindBlob, 2*MaxDecodedSize)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, 2*MaxDecodedSize)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode(buf.Bytes()); !errors.Is(err, ErrTooLarge) {
		t.Errorf("streamed bomb: err = %v, want ErrTooLarge", err)
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()
	id, stored := Encode(KindBlob, []byte("hello"))
	otherID, otherStored := Encode(KindBlob, []byte("world"))

	kind, body, err := DecodeVerify(id, stored)
	if err != nil || kind != KindBlob || string(body) != "hello" {
		t.Errorf("DecodeVerify = %v, %q, %v; want blob, hello, nil", kind, body, err)
	}
	if err := Verify(otherID, stored); !errors.Is(err, ErrHashMismatch) {
		t.Errorf("Verify(wrong id) = %v, want ErrHashMismatch", err)
	}
	if err := Verify(id, otherStored); !errors.Is(err, ErrHashMismatch) {
		t.Errorf("Verify(wrong object) = %v, want ErrHashMismatch", err)
	}
	if err := Verify(id, []byte("junk")); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Verify(junk) = %v, want ErrCorrupt", err)
	}
	// Hash matches but header is invalid: still corrupt.
	raw := []byte("blob 9\x00abc")
	if err := Verify(hash.Sum(nil, raw), compressRaw(raw)); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Verify(matching hash, bad header) = %v, want ErrCorrupt", err)
	}
}

func TestConcurrentEncodeDecode(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := range 64 {
		wg.Go(func() {
			for i := range 50 {
				body := fmt.Appendf(nil, "goroutine %d object %d %s", g, i, bytes.Repeat([]byte{byte(i)}, i*100))
				id, stored := Encode(allKinds[i%len(allKinds)], body)
				kind, got, err := DecodeVerify(id, stored)
				if err != nil {
					errs <- fmt.Errorf("g%d i%d: %w", g, i, err)
					return
				}
				if kind != allKinds[i%len(allKinds)] || !bytes.Equal(got, body) {
					errs <- fmt.Errorf("g%d i%d: got kind %v, %d bytes; want %v, %d bytes", g, i, kind, len(got), allKinds[i%len(allKinds)], len(body))
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestStoredIsSingleFrameAtLevel3(t *testing.T) {
	t.Parallel()
	_, stored := Encode(KindBlob, bytes.Repeat([]byte("abc"), 1000))
	var h zstd.Header
	if err := h.Decode(stored); err != nil {
		t.Fatalf("zstd header: %v", err)
	}
	if !h.HasFCS || h.FrameContentSize != uint64(len("blob 3000\x00")+3000) {
		t.Errorf("frame content size = %v %d, want declared %d", h.HasFCS, h.FrameContentSize, len("blob 3000\x00")+3000)
	}
	if diff := cmp.Diff(zstd.SpeedDefault, zstd.EncoderLevelFromZstd(zstdLevel)); diff != "" {
		t.Errorf("zstd level 3 maps to %v, want SpeedDefault", zstd.EncoderLevelFromZstd(zstdLevel))
	}
}

// 10-testing.md §Property tests: a single-byte mutation of a stored frame never
// passes Verify with different content. Some zstd header bits don't affect the
// decoded bytes (e.g. a larger window descriptor, the "unused" descriptor bit);
// those mutations may verify, but only because the content is unchanged.
func TestVerifyRejectsContentChangingMutations(t *testing.T) {
	t.Parallel()
	bodies := [][]byte{
		{},
		[]byte("hello\n"),
		bytes.Repeat([]byte("compressible "), 200),
	}
	for _, body := range bodies {
		id, stored := Encode(KindBlob, body)
		rejected, total := 0, 0
		for i := range stored {
			for _, flip := range []byte{0x01, 0x02, 0x04, 0x08, 0x10, 0x20, 0x40, 0x80, 0xff} {
				total++
				mutated := bytes.Clone(stored)
				mutated[i] ^= flip
				kind, got, err := DecodeVerify(id, mutated)
				if err != nil {
					rejected++
					continue
				}
				if kind != KindBlob || !bytes.Equal(got, body) {
					t.Errorf("body %d bytes: stored[%d] ^= %#x verified with different content", len(body), i, flip)
				}
			}
		}
		// Sanity: almost every mutation is detected.
		if rejected < total*9/10 {
			t.Errorf("body %d bytes: only %d of %d mutations rejected", len(body), rejected, total)
		}
	}
}

func TestPeekHeader(t *testing.T) {
	t.Parallel()
	big := make([]byte, 20<<20)
	for _, tt := range []struct {
		kind Kind
		body []byte
	}{
		{KindBlob, nil},
		{KindBlob, []byte("hello\n")},
		{KindTree, []byte{0}},
		{KindChangeset, bytes.Repeat([]byte("x"), 1000)},
		{KindBlob, big},
	} {
		_, stored := Encode(tt.kind, tt.body)
		kind, n, err := PeekHeader(stored)
		if err != nil || kind != tt.kind || n != len(tt.body) {
			t.Errorf("PeekHeader(%v, %d bytes) = %v, %d, %v", tt.kind, len(tt.body), kind, n, err)
		}
	}
	for name, stored := range map[string][]byte{
		"not zstd":     []byte("blob 3\x00abc"),
		"no NUL":       compressRaw(bytes.Repeat([]byte("a"), 100)),
		"bad kind":     compressRaw([]byte("file 3\x00abc")),
		"leading zero": compressRaw([]byte("blob 03\x00abc")),
		"empty":        nil,
	} {
		if _, _, err := PeekHeader(stored); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: PeekHeader error = %v, want ErrCorrupt", name, err)
		}
	}
}

func TestPeekHeaderConcurrent(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	for g := range 32 {
		wg.Go(func() {
			for i := range 100 {
				body := bytes.Repeat(fmt.Appendf(nil, "g%d", g), i*37)
				_, stored := Encode(KindBlob, body)
				if _, n, err := PeekHeader(stored); err != nil || n != len(body) {
					t.Errorf("g%d i%d: %d, %v", g, i, n, err)
					return
				}
			}
		})
	}
	wg.Wait()
}
