// Package revision parses and resolves revision syntax (03-data-model.md
// §Revision syntax):
//
//	<hex>            changeset by unique id prefix (≥ 8 hex characters)
//	main, release/x  ref
//	git:<sha>        imported changeset by git sha or unique prefix (≥ 4)
//	@                workspace parent (client-side only)
//	<rev>~N          N-th first-parent ancestor (~ means ~1)
//	<rev>^N          N-th parent, 1-based (^ means ^1, ^0 is <rev> itself)
//
// Suffixes chain left to right, e.g. main~3^2~1.
package revision

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrSyntax reports a malformed revision.
var ErrSyntax = errors.New("invalid revision")

// BaseKind says how a revision's base is named.
type BaseKind int

// Base kinds.
const (
	Name   BaseKind = iota // a ref, or (if no such ref) a hex id prefix
	GitSHA                 // git:<sha or prefix>
	At                     // @, the workspace parent
)

// OpKind is a suffix operator.
type OpKind byte

// Suffix operators.
const (
	Tilde OpKind = '~' // N-th first-parent ancestor
	Caret OpKind = '^' // N-th parent
)

// Op is one suffix: ~N or ^N.
type Op struct {
	Kind OpKind
	N    int
}

// Rev is a parsed revision.
type Rev struct {
	Kind BaseKind
	Base string // ref name, hex prefix or git sha prefix
	Ops  []Op
}

// MinHexPrefix and MinGitPrefix are the shortest accepted id prefixes.
const (
	MinHexPrefix = 8
	MinGitPrefix = 4
)

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)
	hexRe  = regexp.MustCompile(`^[0-9a-fA-F]+$`)
)

// IsHex reports whether s could be a changeset id prefix.
func IsHex(s string) bool {
	return len(s) >= MinHexPrefix && len(s) <= 64 && hexRe.MatchString(s)
}

// Parse parses a revision string.
func Parse(s string) (*Rev, error) {
	if s == "" {
		return nil, fmt.Errorf("%w: empty", ErrSyntax)
	}
	i := strings.IndexAny(s, "~^")
	base, rest := s, ""
	if i >= 0 {
		base, rest = s[:i], s[i:]
	}
	rev := &Rev{}
	switch {
	case base == "@":
		rev.Kind = At
	case strings.HasPrefix(base, "git:"):
		sha := strings.ToLower(base[len("git:"):])
		if len(sha) < MinGitPrefix || len(sha) > 40 || !hexRe.MatchString(sha) {
			return nil, fmt.Errorf("%w %q: git: wants %d to 40 hex characters", ErrSyntax, s, MinGitPrefix)
		}
		rev.Kind, rev.Base = GitSHA, sha
	case nameRe.MatchString(base) || IsHex(base):
		rev.Kind, rev.Base = Name, base
	default:
		return nil, fmt.Errorf("%w %q: %q is not a ref name, id prefix (≥ %d hex) or git:<sha>", ErrSyntax, s, base, MinHexPrefix)
	}
	for len(rest) > 0 {
		op := Op{Kind: OpKind(rest[0]), N: 1}
		rest = rest[1:]
		j := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j > 0 {
			n, err := strconv.Atoi(rest[:j])
			if err != nil || n > 1<<30 {
				return nil, fmt.Errorf("%w %q: count %q out of range", ErrSyntax, s, rest[:j])
			}
			op.N = n
		}
		rest = rest[j:]
		if len(rest) > 0 && rest[0] != '~' && rest[0] != '^' {
			return nil, fmt.Errorf("%w %q: unexpected %q", ErrSyntax, s, rest)
		}
		rev.Ops = append(rev.Ops, op)
	}
	return rev, nil
}

// String formats r in canonical form (counts always explicit).
func (r *Rev) String() string {
	var b strings.Builder
	switch r.Kind {
	case At:
		b.WriteString("@")
	case GitSHA:
		b.WriteString("git:" + r.Base)
	default:
		b.WriteString(r.Base)
	}
	for _, op := range r.Ops {
		b.WriteByte(byte(op.Kind))
		b.WriteString(strconv.Itoa(op.N))
	}
	return b.String()
}
