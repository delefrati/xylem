package treeops

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
)

// ErrStop ends a walk early without an error.
var ErrStop = errors.New("stop walk")

// ComparePathsDFS orders paths as a depth-first walk visits them: component
// by component, each compared bytewise. It differs from plain bytewise order
// (a DFS lists "a/x" before "a-b", while bytes put "a-b" first).
func ComparePathsDFS(a, b []byte) int {
	for {
		ca, ra, moreA := bytes.Cut(a, []byte{'/'})
		cb, rb, moreB := bytes.Cut(b, []byte{'/'})
		if c := bytes.Compare(ca, cb); c != 0 {
			return c
		}
		switch {
		case !moreA && !moreB:
			return 0
		case !moreA:
			return -1
		case !moreB:
			return 1
		}
		a, b = ra, rb
	}
}

// WalkFiles calls fn for every file (non-tree entry) under root, depth-first
// in path order, with paths relative to root. If after is non-empty, files at
// or before after (in ComparePathsDFS order) are skipped without reading the
// subtrees before it. Returning ErrStop from fn ends the walk with no error.
func WalkFiles(ctx context.Context, r TreeReader, root hash.ID, after []byte, fn func(path []byte, e object.Entry) error) error {
	err := walk(ctx, r, root, nil, paths.Split(after), fn)
	if errors.Is(err, ErrStop) {
		return nil
	}
	return err
}

func walk(ctx context.Context, r TreeReader, id hash.ID, prefix []byte, after [][]byte, fn func([]byte, object.Entry) error) error {
	tree, err := r.ReadTree(ctx, id)
	if err != nil {
		return err
	}
	start := 0
	if len(after) > 0 {
		start, _ = slices.BinarySearchFunc(tree.Entries, after[0], func(e object.Entry, name []byte) int {
			return bytes.Compare(e.Name, name)
		})
	}
	for i := start; i < len(tree.Entries); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := tree.Entries[i]
		p := e.Name
		if len(prefix) > 0 {
			p = paths.Join(prefix, e.Name)
		}
		var rest [][]byte
		if len(after) > 0 && bytes.Equal(e.Name, after[0]) {
			if e.Mode != object.ModeTree || len(after) == 1 {
				continue // this entry is the resume point (or contains only earlier files)
			}
			rest = after[1:]
		}
		if e.Mode == object.ModeTree {
			if err := walk(ctx, r, e.ID, p, rest, fn); err != nil {
				return err
			}
			continue
		}
		if err := fn(p, e); err != nil {
			return err
		}
	}
	return nil
}
