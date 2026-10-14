package paths

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestValidName(t *testing.T) {
	t.Parallel()
	valid := []string{
		"a", "file.txt", ".gitignore", "...", ".xylemx", "x.xylem", "-", " ",
		"\xff\xfe", "naïve", "with space", ".Xylem",
	}
	for _, name := range valid {
		if err := ValidName([]byte(name)); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{"", "/", "a/b", "a\x00b", "\x00", ".", "..", ".xylem"}
	for _, name := range invalid {
		if err := ValidName([]byte(name)); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidName(%q) = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestValidPath(t *testing.T) {
	t.Parallel()
	valid := []string{"a", "a/b", "a/b/c.txt", "dir/.gitignore", "\xff/\xfe", "a/.../b"}
	for _, p := range valid {
		if err := ValidPath([]byte(p)); err != nil {
			t.Errorf("ValidPath(%q) = %v, want nil", p, err)
		}
	}
	tests := []struct {
		path     string
		wantName bool // the error also wraps ErrInvalidName
	}{
		{"", false},
		{"/a", true},
		{"a/", true},
		{"a//b", true},
		{"/", true},
		{"a/./b", true},
		{"a/../b", true},
		{"..", true},
		{".xylem/config", true},
		{"a/.xylem", true},
		{"a/.xylem/b", true},
		{"a\x00b", true},
	}
	for _, tt := range tests {
		err := ValidPath([]byte(tt.path))
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("ValidPath(%q) = %v, want ErrInvalidPath", tt.path, err)
		}
		if got := errors.Is(err, ErrInvalidName); got != tt.wantName {
			t.Errorf("ValidPath(%q): errors.Is(ErrInvalidName) = %v, want %v", tt.path, got, tt.wantName)
		}
	}
}

func TestSplitJoin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path  string
		names []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a/b/c", []string{"a", "b", "c"}},
		{"\xff/x", []string{"\xff", "x"}},
	}
	for _, tt := range tests {
		var got []string
		for _, n := range Split([]byte(tt.path)) {
			got = append(got, string(n))
		}
		if diff := cmp.Diff(tt.names, got); diff != "" {
			t.Errorf("Split(%q) mismatch (-want +got):\n%s", tt.path, diff)
		}
		names := make([][]byte, len(tt.names))
		for i, n := range tt.names {
			names[i] = []byte(n)
		}
		if got := Join(names...); string(got) != tt.path {
			t.Errorf("Join(%q) = %q, want %q", tt.names, got, tt.path)
		}
	}
}

func TestJoinDoesNotAlias(t *testing.T) {
	t.Parallel()
	a := []byte("a")
	got := Join(a)
	got[0] = 'z'
	if string(a) != "a" {
		t.Errorf("Join aliased its input: %q", a)
	}
}

func TestParentBase(t *testing.T) {
	t.Parallel()
	tests := []struct{ path, parent, base string }{
		{"a", "", "a"},
		{"a/b", "a", "b"},
		{"a/b/c.txt", "a/b", "c.txt"},
		{"", "", ""},
	}
	for _, tt := range tests {
		if got := Parent([]byte(tt.path)); string(got) != tt.parent {
			t.Errorf("Parent(%q) = %q, want %q", tt.path, got, tt.parent)
		}
		if got := Base([]byte(tt.path)); string(got) != tt.base {
			t.Errorf("Base(%q) = %q, want %q", tt.path, got, tt.base)
		}
	}
}

func TestIsUnder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path, dir string
		want      bool
	}{
		{"a", "", true},
		{"a/b", "", true},
		{"a", "a", true},
		{"a/b", "a", true},
		{"a/b/c", "a", true},
		{"a/b/c", "a/b", true},
		{"services/x", "serv", false},
		{"serv", "services", false},
		{"ab", "a", false},
		{"a", "a/b", false},
		{"b/a", "a", false},
		{"a-b/c", "a", false},
		{"a0", "a", false},
	}
	for _, tt := range tests {
		if got := IsUnder([]byte(tt.path), []byte(tt.dir)); got != tt.want {
			t.Errorf("IsUnder(%q, %q) = %v, want %v", tt.path, tt.dir, got, tt.want)
		}
	}
}

func TestCompare(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b string
		want int
	}{
		{"a", "a", 0},
		{"a", "b", -1},
		{"b", "a", 1},
		// Bytewise, not git order: "a-b" < "a/b" < "a0" because '-' < '/' < '0'.
		{"a-b", "a/b", -1},
		{"a/b", "a0", -1},
		{"A", "a", -1},
		{"z", "\xff", -1},
	}
	for _, tt := range tests {
		if got := Compare([]byte(tt.a), []byte(tt.b)); got != tt.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestDirPrefixRange(t *testing.T) {
	t.Parallel()
	lo, hi := DirPrefixRange([]byte("a/b"))
	if string(lo) != "a/b/" || string(hi) != "a/b0" {
		t.Errorf("DirPrefixRange(a/b) = %q, %q; want \"a/b/\", \"a/b0\"", lo, hi)
	}
	if lo, hi := DirPrefixRange(nil); lo != nil || hi != nil {
		t.Errorf("DirPrefixRange(root) = %q, %q; want nil, nil", lo, hi)
	}

	// For valid paths, being in the range must mean exactly "under dir and not dir".
	inRange := func(p, lo, hi []byte) bool {
		return (lo == nil || bytes.Compare(p, lo) >= 0) && (hi == nil || bytes.Compare(p, hi) < 0)
	}
	paths := []string{
		"a", "a-b", "a.b", "a0", "a/b", "a/b/c", "a/b-c", "a/b.c", "a/b0", "a/b/\xff",
		"a/bb", "a/c", "b", "\xff", "a/b/c/d",
	}
	dirs := []string{"", "a", "a/b", "a/b/c", "b", "\xff", "z"}
	for _, d := range dirs {
		lo, hi := DirPrefixRange([]byte(d))
		for _, p := range paths {
			want := IsUnder([]byte(p), []byte(d)) && p != d
			if got := inRange([]byte(p), lo, hi); got != want {
				t.Errorf("dir %q, path %q: in range = %v, want %v", d, p, got, want)
			}
		}
	}
}

func TestDirPrefixRangeDoesNotAlias(t *testing.T) {
	t.Parallel()
	dir := make([]byte, 1, 16)
	dir[0] = 'a'
	lo, hi := DirPrefixRange(dir)
	if string(lo) != "a/" || string(hi) != "a0" {
		t.Errorf("DirPrefixRange with spare capacity = %q, %q; want \"a/\", \"a0\"", lo, hi)
	}
}

// Valid paths round-trip through Split and Join, every component is a valid
// name, and joining valid names gives a valid path.
func FuzzValidPath(f *testing.F) {
	for _, seed := range []string{"", "a", "a/b", "/a", "a/", "a//b", ".xylem", "a/./b", "\xff/\x00", "a/b/c.txt"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, path []byte) {
		names := Split(path)
		allValid := len(names) > 0
		for _, n := range names {
			if ValidName(n) != nil {
				allValid = false
			}
		}
		err := ValidPath(path)
		if (err == nil) != allValid {
			t.Fatalf("ValidPath(%q) = %v, but components all valid = %v", path, err, allValid)
		}
		if err != nil {
			return
		}
		if got := Join(names...); !bytes.Equal(got, path) {
			t.Fatalf("Join(Split(%q)) = %q", path, got)
		}
		if len(names) > 1 {
			if !IsUnder(path, Parent(path)) || !bytes.Equal(Join(Parent(path), Base(path)), path) {
				t.Fatalf("Parent/Base of %q: %q, %q", path, Parent(path), Base(path))
			}
		}
	})
}
