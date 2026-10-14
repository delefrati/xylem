package changeset

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzDecodeChangeset: Decode never panics, and any accepted input re-encodes
// to identical bytes. ValidateShape and Summary must not panic on it either.
func FuzzDecodeChangeset(f *testing.F) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "testdata", "golden", "changeset_*.bin"))
	if err != nil || len(paths) == 0 {
		f.Fatalf("no golden changeset vectors: %v", err)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		c, err := Decode(body)
		if err != nil {
			return
		}
		again, err := c.Encode()
		if err != nil {
			t.Fatalf("decoded changeset does not encode: %v", err)
		}
		if !bytes.Equal(again, body) {
			t.Fatalf("re-encode differs:\n in  %x\n out %x", body, again)
		}
		_ = c.ValidateShape(ShapeOptions{})
		s := c.Summary()
		if !utf8.ValidString(s) || strings.Contains(s, "\x00") || utf8.RuneCountInString(s) > MaxSummaryLength {
			t.Fatalf("bad summary %q", s)
		}
	})
}
