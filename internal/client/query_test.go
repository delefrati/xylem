package client

import (
	"testing"

	"github.com/delefrati/xylem/internal/hash"
)

func TestResolveRev(t *testing.T) {
	t.Parallel()
	parent := hash.Sum([]byte("blob 1\x00"), []byte("x"))
	w := &Workspace{State: State{Parent: parent}}
	for _, tc := range []struct{ in, want string }{
		{"@", parent.String()},
		{"@~2", parent.String() + "~2"},
		{"@^2", parent.String() + "^2"},
		{"main", "main"},
		{"@foo", "@foo"}, // not "@": passed to the server as is
		{"git:abc", "git:abc"},
	} {
		if got, err := ResolveRev(tc.in, w); err != nil || got != tc.want {
			t.Errorf("ResolveRev(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := ResolveRev("@", nil); ExitCode(err) != ExitUsage {
		t.Errorf("@ without a workspace: %v", err)
	}
	if _, err := ResolveRev("@", &Workspace{}); err == nil {
		t.Error("@ in a workspace without a parent succeeded")
	}
}
