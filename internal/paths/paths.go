// Package paths validates path components and provides split, join, ordering
// and IsUnder helpers over byte paths.
//
// Paths are raw bytes (ADR-015) and need not be UTF-8. A name is a non-empty
// byte string with no '/' and no NUL that is not ".", ".." or ".xylem". A path
// is one or more names joined by '/'. Ordering is plain bytewise comparison.
// See 03-data-model.md §Paths and names.
//
// The empty path denotes the repository root where a directory is expected
// (IsUnder, DirPrefixRange); it is not a valid file path.
package paths

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

// Reserved is the workspace metadata directory name, which no path may contain.
const Reserved = ".xylem"

var (
	// ErrInvalidName reports a name that breaks the naming rules.
	ErrInvalidName = errors.New("invalid name")
	// ErrInvalidPath reports a path that breaks the path rules.
	ErrInvalidPath = errors.New("invalid path")
)

// ValidName returns nil if name is a valid path component, or an error wrapping
// ErrInvalidName that says which rule it breaks.
func ValidName(name []byte) error {
	var reason string
	switch {
	case len(name) == 0:
		reason = "empty"
	case bytes.IndexByte(name, '/') >= 0:
		reason = "contains '/'"
	case bytes.IndexByte(name, 0) >= 0:
		reason = "contains NUL"
	case string(name) == "." || string(name) == "..":
		reason = "is a relative component"
	case string(name) == Reserved:
		reason = "is reserved"
	default:
		return nil
	}
	return fmt.Errorf("%w: %s %s", ErrInvalidName, strconv.Quote(string(name)), reason)
}

// ValidPath returns nil if path is a valid non-empty path, or an error wrapping
// ErrInvalidPath (and ErrInvalidName for a bad component) that says why.
func ValidPath(path []byte) error {
	if len(path) == 0 {
		return fmt.Errorf("%w: empty", ErrInvalidPath)
	}
	for rest := path; ; {
		name, tail, more := bytes.Cut(rest, []byte{'/'})
		if err := ValidName(name); err != nil {
			return fmt.Errorf("%w %s: %w", ErrInvalidPath, strconv.Quote(string(path)), err)
		}
		if !more {
			return nil
		}
		rest = tail
	}
}

// Split returns the components of path. The results alias path. The root
// (empty path) has no components. Split does not validate.
func Split(path []byte) [][]byte {
	if len(path) == 0 {
		return nil
	}
	return bytes.Split(path, []byte{'/'})
}

// Join joins names with '/' into a newly allocated path. Join of no names is
// the root (empty path). Join does not validate.
func Join(names ...[]byte) []byte {
	return bytes.Join(names, []byte{'/'})
}

// Parent returns the directory containing path: everything before the last
// '/', or the root (empty) for a top-level path. The result aliases path.
func Parent(path []byte) []byte {
	i := bytes.LastIndexByte(path, '/')
	if i < 0 {
		return path[:0]
	}
	return path[:i]
}

// Base returns the last component of path. The result aliases path.
func Base(path []byte) []byte {
	return path[bytes.LastIndexByte(path, '/')+1:]
}

// IsUnder reports whether path is dir itself or lies below it, comparing whole
// components ("serv" is not under "services"). Every path is under the root
// (empty dir).
func IsUnder(path, dir []byte) bool {
	if len(dir) == 0 {
		return true
	}
	if !bytes.HasPrefix(path, dir) {
		return false
	}
	return len(path) == len(dir) || path[len(dir)] == '/'
}

// Compare orders paths or names bytewise, returning -1, 0 or +1.
func Compare(a, b []byte) int {
	return bytes.Compare(a, b)
}

// DirPrefixRange returns the byte range of paths strictly below dir, for
// queries of the form path >= lo AND path < hi, where a nil bound is open.
// For a non-root dir that is [dir+"/", dir+"0"), since '0' is the byte after
// '/'. For the root both bounds are nil: every path is below it. dir itself is
// not in the range.
func DirPrefixRange(dir []byte) (lo, hi []byte) {
	if len(dir) == 0 {
		return nil, nil
	}
	lo = append(bytes.Clone(dir), '/')
	hi = append(bytes.Clone(dir), '/'+1)
	return lo, hi
}
