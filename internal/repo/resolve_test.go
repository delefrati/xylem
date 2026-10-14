package repo

import (
	"errors"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/hash"
)

func TestPickOne(t *testing.T) {
	t.Parallel()
	name := func(id hash.ID) string { return id.Short(8) }
	if _, err := pickOne("x", nil, name); !errors.Is(err, ErrNotFound) {
		t.Errorf("none: %v", err)
	}
	a, b := hash.ID{1}, hash.ID{2}
	if got, err := pickOne("x", []hash.ID{a}, name); err != nil || got != a {
		t.Errorf("one: %v %v", got, err)
	}
	_, err := pickOne("abcd1234", []hash.ID{a, b}, name)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), a.Short(8)) || !strings.Contains(err.Error(), b.Short(8)) {
		t.Errorf("two: %v", err)
	}
	five := []hash.ID{{1}, {2}, {3}, {4}, {5}}
	if _, err := pickOne("x", five, name); !strings.HasSuffix(err.Error(), "…") {
		t.Errorf("five candidates should note there may be more: %v", err)
	}
}

func TestGitPrefixRange(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ in, lo, hi string }{
		{"ab", "\xab", "\xac"},
		{"abc", "\xab\xc0", "\xab\xd0"},
		{"ff", "\xff", ""},
		{"abf", "\xab\xf0", "\xac\x00"},
	} {
		lo, hi, err := gitPrefixRange(tt.in)
		if err != nil || string(lo) != tt.lo || string(hi) != tt.hi || (tt.hi == "" && hi != nil) {
			t.Errorf("gitPrefixRange(%q) = %x, %x, %v", tt.in, lo, hi, err)
		}
	}
	for _, bad := range []string{"", "xyz", strings.Repeat("a", 41)} {
		if _, _, err := gitPrefixRange(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("gitPrefixRange(%q) = %v", bad, err)
		}
	}
}
