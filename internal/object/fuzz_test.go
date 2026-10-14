package object

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/delefrati/xylem/internal/hash"
)

// goldenBodies returns the approved golden bodies of the given kind prefix as
// fuzz seeds.
func goldenBodies(f *testing.F, prefix string) [][]byte {
	f.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "testdata", "golden", prefix+"_*.bin"))
	if err != nil || len(paths) == 0 {
		f.Fatalf("no golden %s vectors: %v", prefix, err)
	}
	var bodies [][]byte
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		bodies = append(bodies, b)
	}
	return bodies
}

// FuzzDecodeTree: DecodeTree never panics, and any accepted input re-encodes
// to identical bytes.
func FuzzDecodeTree(f *testing.F) {
	for _, b := range goldenBodies(f, "tree") {
		f.Add(b)
	}
	f.Add([]byte{0x80, 0x00})
	f.Fuzz(func(t *testing.T, body []byte) {
		tree, err := DecodeTree(body)
		if err != nil {
			return
		}
		again, err := EncodeTree(tree)
		if err != nil {
			t.Fatalf("decoded tree does not encode: %v", err)
		}
		if !bytes.Equal(again, body) {
			t.Fatalf("re-encode differs:\n in  %x\n out %x", body, again)
		}
		for _, e := range tree.Entries {
			if got, ok := tree.Lookup(e.Name); !ok || !bytes.Equal(got.Name, e.Name) {
				t.Fatalf("Lookup(%q) failed on a decoded tree", e.Name)
			}
		}
	})
}

// FuzzObjectDecode: Decode never panics, and any accepted stored frame
// re-encodes to the same kind and body and verifies against its content id.
// Frames themselves are not canonical (see 10-testing.md), so bytes of the
// frame may differ.
func FuzzObjectDecode(f *testing.F) {
	for _, prefix := range []string{"blob", "tree", "changeset"} {
		kind, _ := parseKind([]byte(prefix))
		for _, b := range goldenBodies(f, prefix) {
			_, stored := Encode(kind, b)
			f.Add(stored)
		}
	}
	f.Add(compressRaw([]byte("blob 03\x00abc")))
	f.Fuzz(func(t *testing.T, stored []byte) {
		kind, body, err := Decode(stored)
		if err != nil {
			return
		}
		id, again := Encode(kind, body)
		kind2, body2, err := Decode(again)
		if err != nil || kind2 != kind || !bytes.Equal(body2, body) {
			t.Fatalf("re-encode of %v (%d bytes) does not decode back: %v", kind, len(body), err)
		}
		if err := Verify(id, stored); err != nil {
			t.Fatalf("accepted frame does not verify against its content id: %v", err)
		}
		if id != hash.Sum(Header(kind, len(body)), body) {
			t.Fatal("Encode id differs from hash of header and body")
		}
	})
}
