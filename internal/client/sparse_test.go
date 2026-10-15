package client

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestSparseProfileChanges(t *testing.T) {
	t.Parallel()
	ps := func(s ...string) []Path {
		out := make([]Path, len(s))
		for i, p := range s {
			out[i] = Path(p)
		}
		return out
	}
	add := []struct{ profile, dirs, want []Path }{
		{ps("a"), ps("b"), ps("a", "b")},
		{ps("a"), ps("a/x"), ps("a")},                  // covered
		{ps("a/x", "b", "a/y"), ps("a"), ps("b", "a")}, // replaces entries under it
		{ps("ab"), ps("a"), ps("ab", "a")},             // whole components
		{ps("a"), ps("b", "b"), ps("a", "b")},
	}
	for _, tc := range add {
		got, err := SparseAdd(tc.profile, tc.dirs)
		if err != nil || !cmp.Equal(got, tc.want) {
			t.Errorf("SparseAdd(%q, %q) = %q, %v; want %q", tc.profile, tc.dirs, got, err, tc.want)
		}
	}
	if got, err := SparseRemove(ps("a", "b", "c"), ps("b", "a")); err != nil || !cmp.Equal(got, ps("c")) {
		t.Errorf("SparseRemove = %q, %v", got, err)
	}
	for _, tc := range []struct{ profile, dirs []Path }{
		{ps("a", "b"), ps("c")},   // not an entry
		{ps("a", "b"), ps("a/x")}, // under an entry, not an entry
		{ps("a"), ps("a")},        // last entry
		{nil, ps("a")},            // full checkout
	} {
		if _, err := SparseRemove(tc.profile, tc.dirs); ExitCode(err) != ExitUsage {
			t.Errorf("SparseRemove(%q, %q): %v", tc.profile, tc.dirs, err)
		}
	}
	if _, err := SparseAdd(nil, ps("a")); ExitCode(err) != ExitUsage {
		t.Errorf("SparseAdd on a full checkout: %v", err)
	}
}
