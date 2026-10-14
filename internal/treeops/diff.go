package treeops

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
)

// ErrTooLarge reports a diff that scanned more tree entries than allowed.
var ErrTooLarge = errors.New("diff too large")

// Change is a file that differs between two trees. Old or New has Mode 0
// when the path does not exist on that side.
type Change struct {
	Path []byte
	Old  object.Entry
	New  object.Entry
}

// Diff returns the files that differ between trees a and b (zero ids mean
// the empty tree), in depth-first path order. Identical subtrees are skipped.
// If prefixes is non-empty, only paths at or under one of them are compared.
// maxEntries (> 0) bounds the tree entries read; beyond it Diff fails with
// ErrTooLarge.
func Diff(ctx context.Context, r TreeReader, a, b hash.ID, prefixes [][]byte, maxEntries int) ([]Change, error) {
	d := &differ{ctx: ctx, r: r, prefixes: prefixes, budget: maxEntries, limited: maxEntries > 0}
	if err := d.trees(a, b, nil); err != nil {
		return nil, err
	}
	return d.out, nil
}

type differ struct {
	ctx      context.Context
	r        TreeReader
	prefixes [][]byte
	budget   int  // tree entries that may still be read
	limited  bool // whether budget applies
	out      []Change
}

// relevant reports whether a directory or file at p may contain or be a
// filtered path.
func (d *differ) relevant(p []byte, dir bool) bool {
	if len(d.prefixes) == 0 {
		return true
	}
	for _, pre := range d.prefixes {
		if paths.IsUnder(p, pre) || (dir && paths.IsUnder(pre, p)) {
			return true
		}
	}
	return false
}

func (d *differ) read(id hash.ID) (*object.Tree, error) {
	if id.IsZero() {
		return &object.Tree{}, nil
	}
	if err := d.ctx.Err(); err != nil {
		return nil, err
	}
	t, err := d.r.ReadTree(d.ctx, id)
	if err != nil {
		return nil, err
	}
	if d.limited {
		if d.budget -= len(t.Entries); d.budget < 0 {
			return nil, fmt.Errorf("%w: more tree entries than allowed", ErrTooLarge)
		}
	}
	return t, nil
}

func join(prefix, name []byte) []byte {
	if len(prefix) == 0 {
		return bytes.Clone(name)
	}
	return paths.Join(prefix, name)
}

func (d *differ) trees(a, b hash.ID, prefix []byte) error {
	if a == b {
		return nil
	}
	ta, err := d.read(a)
	if err != nil {
		return err
	}
	tb, err := d.read(b)
	if err != nil {
		return err
	}
	i, j := 0, 0
	for i < len(ta.Entries) || j < len(tb.Entries) {
		var ea, eb *object.Entry
		switch {
		case i == len(ta.Entries):
			eb = &tb.Entries[j]
			j++
		case j == len(tb.Entries):
			ea = &ta.Entries[i]
			i++
		default:
			switch c := bytes.Compare(ta.Entries[i].Name, tb.Entries[j].Name); {
			case c < 0:
				ea = &ta.Entries[i]
				i++
			case c > 0:
				eb = &tb.Entries[j]
				j++
			default:
				ea, eb = &ta.Entries[i], &tb.Entries[j]
				i++
				j++
			}
		}
		if err := d.entry(prefix, ea, eb); err != nil {
			return err
		}
	}
	return nil
}

// entry compares one name present in a, b or both.
func (d *differ) entry(prefix []byte, ea, eb *object.Entry) error {
	var name []byte
	if ea != nil {
		name = ea.Name
	} else {
		name = eb.Name
	}
	p := join(prefix, name)
	aTree := ea != nil && ea.Mode == object.ModeTree
	bTree := eb != nil && eb.Mode == object.ModeTree
	if !d.relevant(p, aTree || bTree) {
		return nil
	}
	if aTree || bTree {
		// A file on the other side comes first ("x" sorts before "x/…" in
		// depth-first order); then the directory side(s) are compared, so
		// dir ↔ file appears as deletes plus an add, as in git.
		if !aTree && ea != nil {
			d.emit(p, *ea, object.Entry{})
		}
		if !bTree && eb != nil {
			d.emit(p, object.Entry{}, *eb)
		}
		var ida, idb hash.ID
		if aTree {
			ida = ea.ID
		}
		if bTree {
			idb = eb.ID
		}
		return d.trees(ida, idb, p)
	}
	switch {
	case ea == nil:
		d.emit(p, object.Entry{}, *eb)
	case eb == nil:
		d.emit(p, *ea, object.Entry{})
	case ea.ID != eb.ID || ea.Mode != eb.Mode:
		d.emit(p, *ea, *eb)
	}
	return nil
}

func (d *differ) emit(p []byte, old, new object.Entry) {
	if len(d.prefixes) > 0 && !d.relevant(p, false) {
		return
	}
	d.out = append(d.out, Change{Path: p, Old: old, New: new})
}
