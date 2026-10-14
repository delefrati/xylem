package revision

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParse(t *testing.T) {
	t.Parallel()
	hex64 := strings.Repeat("ab", 32)
	tests := []struct {
		in   string
		want Rev
		str  string
	}{
		{"main", Rev{Kind: Name, Base: "main"}, "main"},
		{"release/2026.10", Rev{Kind: Name, Base: "release/2026.10"}, "release/2026.10"},
		{"main~3", Rev{Kind: Name, Base: "main", Ops: []Op{{Tilde, 3}}}, "main~3"},
		{"main~", Rev{Kind: Name, Base: "main", Ops: []Op{{Tilde, 1}}}, "main~1"},
		{"main^", Rev{Kind: Name, Base: "main", Ops: []Op{{Caret, 1}}}, "main^1"},
		{"main^0", Rev{Kind: Name, Base: "main", Ops: []Op{{Caret, 0}}}, "main^0"},
		{"main~3^2~1", Rev{Kind: Name, Base: "main", Ops: []Op{{Tilde, 3}, {Caret, 2}, {Tilde, 1}}}, "main~3^2~1"},
		{"main^^", Rev{Kind: Name, Base: "main", Ops: []Op{{Caret, 1}, {Caret, 1}}}, "main^1^1"},
		{"deadbeef", Rev{Kind: Name, Base: "deadbeef"}, "deadbeef"},
		{"DEADBEEF01", Rev{Kind: Name, Base: "DEADBEEF01"}, "DEADBEEF01"},
		{hex64 + "~2", Rev{Kind: Name, Base: hex64, Ops: []Op{{Tilde, 2}}}, hex64 + "~2"},
		{"git:abc1", Rev{Kind: GitSHA, Base: "abc1"}, "git:abc1"},
		{"git:ABCDEF0123", Rev{Kind: GitSHA, Base: "abcdef0123"}, "git:abcdef0123"},
		{"git:" + strings.Repeat("f", 40) + "^2", Rev{Kind: GitSHA, Base: strings.Repeat("f", 40), Ops: []Op{{Caret, 2}}}, "git:" + strings.Repeat("f", 40) + "^2"},
		{"@", Rev{Kind: At}, "@"},
		{"@~1", Rev{Kind: At, Ops: []Op{{Tilde, 1}}}, "@~1"},
	}
	for _, tt := range tests {
		got, err := Parse(tt.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.in, err)
			continue
		}
		if diff := cmp.Diff(&tt.want, got); diff != "" {
			t.Errorf("Parse(%q) (-want +got):\n%s", tt.in, diff)
		}
		if got.String() != tt.str {
			t.Errorf("Parse(%q).String() = %q, want %q", tt.in, got.String(), tt.str)
		}
		if again, err := Parse(got.String()); err != nil || again.String() != got.String() {
			t.Errorf("String() of %q does not reparse: %v", tt.in, err)
		}
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"", "~1", "^", "Main", "-x", "main~x", "main^2x",
		"git:", "git:abc", "git:xyz1", "git:" + strings.Repeat("a", 41), "main~99999999999",
		"a b", "main:path", "ma in",
	} {
		if _, err := Parse(in); !errors.Is(err, ErrSyntax) {
			t.Errorf("Parse(%q) = %v, want ErrSyntax", in, err)
		}
	}
}

func TestIsHex(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"deadbeef": true, "DEADBEEF": true, "deadbee": false, "deadbeeg": false,
		strings.Repeat("a", 64): true, strings.Repeat("a", 65): false, "main": false,
	} {
		if IsHex(in) != want {
			t.Errorf("IsHex(%q) = %v", in, !want)
		}
	}
}
