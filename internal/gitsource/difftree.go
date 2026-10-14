package gitsource

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
)

// Git file modes as they appear in diff-tree and ls-tree output.
const (
	ModeFile      uint32 = 0o100644
	ModeExec      uint32 = 0o100755
	ModeSymlink   uint32 = 0o120000
	ModeSubmodule uint32 = 0o160000
	ModeTree      uint32 = 0o040000
)

// Change is one entry of `diff-tree --raw`: status A (added), M (modified),
// D (deleted) or T (type changed). Modes and shas are zero on the side where
// the path does not exist.
type Change struct {
	Status  byte
	OldMode uint32
	NewMode uint32
	OldSHA  SHA
	NewSHA  SHA
	Path    []byte
}

// DiffTree returns the changes from parent to commit, recursively and
// without rename detection. A nil parent diffs a root commit against the
// empty tree.
func (r *Repo) DiffTree(ctx context.Context, parent *SHA, commit SHA) ([]Change, error) {
	args := []string{"diff-tree", "-r", "-z", "--raw", "--no-renames", "--no-commit-id", "--full-index"}
	if parent == nil {
		args = append(args, "--root", commit.String())
	} else {
		args = append(args, parent.String(), commit.String())
	}
	out, err := r.output(ctx, args...)
	if err != nil {
		return nil, err
	}
	changes, err := parseRawDiff(out)
	if err != nil {
		return nil, fmt.Errorf("diff-tree %s: %w", commit, err)
	}
	return changes, nil
}

// parseRawDiff parses -z --raw output: ":<old mode> <new mode> <old sha>
// <new sha> <status>\0<path>\0" repeated.
func parseRawDiff(out []byte) ([]Change, error) {
	var changes []Change
	for len(out) > 0 {
		meta, rest, ok := bytes.Cut(out, []byte{0})
		if !ok || len(meta) == 0 || meta[0] != ':' {
			return nil, fmt.Errorf("malformed raw diff entry %q", meta)
		}
		path, rest, ok := bytes.Cut(rest, []byte{0})
		if !ok {
			return nil, fmt.Errorf("raw diff entry %q has no path", meta)
		}
		fields := bytes.Fields(meta[1:])
		if len(fields) != 5 || len(fields[4]) != 1 {
			return nil, fmt.Errorf("malformed raw diff entry %q", meta)
		}
		ch := Change{Status: fields[4][0], Path: bytes.Clone(path)}
		var err error
		if ch.OldMode, err = parseMode(fields[0]); err != nil {
			return nil, err
		}
		if ch.NewMode, err = parseMode(fields[1]); err != nil {
			return nil, err
		}
		if ch.OldSHA, err = ParseSHA(string(fields[2])); err != nil {
			return nil, err
		}
		if ch.NewSHA, err = ParseSHA(string(fields[3])); err != nil {
			return nil, err
		}
		switch ch.Status {
		case 'A', 'M', 'D', 'T':
		default:
			return nil, fmt.Errorf("unexpected diff status %q for %q", ch.Status, path)
		}
		changes = append(changes, ch)
		out = rest
	}
	return changes, nil
}

func parseMode(b []byte) (uint32, error) {
	m, err := strconv.ParseUint(string(b), 8, 32)
	if err != nil {
		return 0, fmt.Errorf("bad mode %q", b)
	}
	return uint32(m), nil
}

// TreeEntry is one line of `ls-tree -r -l`.
type TreeEntry struct {
	Mode uint32
	SHA  SHA
	Size int64 // -1 for submodules
	Path []byte
}

// LsTree lists every file in commit's tree recursively, with sizes
// (`ls-tree -r -z -l --full-tree`).
func (r *Repo) LsTree(ctx context.Context, commit SHA) ([]TreeEntry, error) {
	out, err := r.output(ctx, "ls-tree", "-r", "-z", "-l", "--full-tree", "--full-name", commit.String())
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for _, rec := range bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, path, ok := bytes.Cut(rec, []byte{'\t'})
		fields := bytes.Fields(meta)
		if !ok || len(fields) != 4 {
			return nil, fmt.Errorf("malformed ls-tree entry %q", rec)
		}
		var e TreeEntry
		if e.Mode, err = parseMode(fields[0]); err != nil {
			return nil, err
		}
		if e.SHA, err = ParseSHA(string(fields[2])); err != nil {
			return nil, err
		}
		if string(fields[3]) == "-" {
			e.Size = -1
		} else if e.Size, err = strconv.ParseInt(string(fields[3]), 10, 64); err != nil {
			return nil, fmt.Errorf("malformed ls-tree size in %q", rec)
		}
		e.Path = bytes.Clone(path)
		entries = append(entries, e)
	}
	return entries, nil
}
