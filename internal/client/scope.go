package client

import (
	"github.com/delefrati/xylem/internal/paths"
)

// Scope is a sparse profile (06 §Fetch trees and blobs). Comparisons are by
// whole path components: "serv" is neither under nor an ancestor of
// "services". An empty scope is the full checkout.
type Scope struct {
	dirs [][]byte
}

// NewScope returns the scope of the given sparse directories.
func NewScope(dirs [][]byte) Scope {
	return Scope{dirs: dirs}
}

// Full reports whether the scope is the whole tree.
func (s Scope) Full() bool { return len(s.dirs) == 0 }

// Dirs returns the sparse directories.
func (s Scope) Dirs() [][]byte { return s.dirs }

// TreeInScope reports whether directory dir must be fetched: it is an
// ancestor of a sparse directory (needed to reach it) or equal to or under
// one.
func (s Scope) TreeInScope(dir []byte) bool {
	if s.Full() || len(dir) == 0 {
		return true
	}
	for _, d := range s.dirs {
		if paths.IsUnder(dir, d) || paths.IsUnder(d, dir) {
			return true
		}
	}
	return false
}

// FileInScope reports whether file path is materialized: it is under a
// sparse directory, or at the root (root files are always in scope).
func (s Scope) FileInScope(path []byte) bool {
	if s.Full() || len(paths.Parent(path)) == 0 {
		return true
	}
	for _, d := range s.dirs {
		if paths.IsUnder(path, d) {
			return true
		}
	}
	return false
}
