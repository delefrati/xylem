package repo

import (
	"bytes"
	"context"
	"fmt"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/derived"
	"github.com/delefrati/xylem/internal/diff"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
)

// BlameResult is the blame of a file: one entry per line.
type BlameResult struct {
	Lines    []derived.BlameLine
	Content  [][]byte  // the file's lines, with their terminators
	Modifier hash.ID   // the last changeset at or before the revision that changed the file
	Origins  []hash.ID // distinct origins, in order of first appearance
}

// modification is a changeset's change to one exact path.
type modification struct {
	id     hash.ID
	change changeset.Change
}

// Blame returns the blame of the file at path as of changeset at, following
// first-parent history (04 §Kind 4). Values are computed lazily, stored per
// modifying changeset and reused.
func (r *Repo) Blame(ctx context.Context, at hash.ID, path []byte) (*BlameResult, error) {
	if err := paths.ValidPath(path); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	last, ok, err := r.lastModifier(ctx, at, path)
	if err != nil {
		return nil, err
	}
	if !ok || last.change.Op != changeset.OpPut {
		return nil, fmt.Errorf("%w: %q does not exist at %s", ErrNotFound, path, at.Short(12))
	}
	content, err := r.blob(ctx, last.change.Blob)
	if err != nil {
		return nil, err
	}
	if diff.IsBinary(content) {
		return nil, fmt.Errorf("%w: %q", ErrBinary, path)
	}
	value, err := r.engine.Get(ctx, derived.BlameKind, last.id, path, func(ctx context.Context) ([]byte, error) {
		lines, err := r.computeBlame(ctx, last, path)
		if err != nil {
			return nil, err
		}
		return derived.EncodeBlame(lines), nil
	})
	if err != nil {
		return nil, err
	}
	lines, err := derived.DecodeBlame(value)
	if err != nil {
		return nil, err
	}
	res := &BlameResult{Lines: lines, Content: diff.Lines(content), Modifier: last.id}
	if len(res.Content) != len(lines) {
		return nil, fmt.Errorf("stored blame of %q at %s has %d lines, the file %d", path, last.id.Short(12), len(lines), len(res.Content))
	}
	seen := map[hash.ID]bool{}
	for _, l := range lines {
		if !seen[l.Origin] {
			seen[l.Origin] = true
			res.Origins = append(res.Origins, l.Origin)
		}
	}
	return res, nil
}

// computeBlame computes blame(last, path) iteratively: walk back through
// earlier modifications of path until a stored blame or a fresh start
// (no earlier version, a delete, or a file ↔ symlink change), then compute
// forward, storing each value on the way.
func (r *Repo) computeBlame(ctx context.Context, last modification, path []byte) ([]derived.BlameLine, error) {
	chain := []modification{last} // newest first
	var (
		base     []derived.BlameLine // stored blame of the modification before the chain's oldest
		baseMod  modification
		haveBase bool
	)
	for {
		cur := chain[len(chain)-1]
		parent, err := r.fpParent(ctx, cur.id)
		if err != nil {
			return nil, err
		}
		if parent.IsZero() {
			break
		}
		prev, ok, err := r.lastModifier(ctx, parent, path)
		if err != nil {
			return nil, err
		}
		if !ok || prev.change.Op == changeset.OpDelete || isSymlink(prev.change.Mode) != isSymlink(cur.change.Mode) {
			break // fresh start at cur
		}
		if v, found, err := r.engine.Load(ctx, derived.BlameKind, prev.id, path); err != nil {
			return nil, err
		} else if found {
			if base, err = derived.DecodeBlame(v); err != nil {
				return nil, err
			}
			baseMod, haveBase = prev, true
			break
		}
		chain = append(chain, prev)
	}

	var (
		prevLines   []derived.BlameLine
		prevContent []byte
		havePrev    bool
	)
	if haveBase {
		c, err := r.blob(ctx, baseMod.change.Blob)
		if err != nil {
			return nil, err
		}
		prevLines, prevContent, havePrev = base, c, true
	}
	for i := len(chain) - 1; i >= 0; i-- {
		m := chain[i]
		content, err := r.blob(ctx, m.change.Blob)
		if err != nil {
			return nil, err
		}
		lines := derived.FreshBlame(m.id, len(diff.Lines(content)))
		if havePrev {
			if lines, err = derived.NextBlame(prevLines, prevContent, content, m.id); err != nil {
				return nil, err
			}
		}
		if i > 0 { // the newest is stored by the engine
			if err := r.engine.Store(ctx, derived.BlameKind, m.id, path, derived.EncodeBlame(lines)); err != nil {
				return nil, err
			}
		}
		prevLines, prevContent, havePrev = lines, content, true
	}
	return prevLines, nil
}

func isSymlink(m object.Mode) bool { return m == object.ModeSymlink }

// lastModifier returns the newest changeset at or before start (on its
// first-parent chain) whose changes include exactly path.
func (r *Repo) lastModifier(ctx context.Context, start hash.ID, path []byte) (modification, bool, error) {
	cur, err := r.StartLog(ctx, start)
	if err != nil {
		return modification{}, false, err
	}
	for {
		ids, next, err := r.Log(ctx, cur, path, 16)
		if err != nil {
			return modification{}, false, err
		}
		found, err := r.Changesets(ctx, ids)
		if err != nil {
			return modification{}, false, err
		}
		for _, id := range ids { // newest first
			for _, ch := range found[id].Changes {
				if bytes.Equal(ch.Path, path) {
					return modification{id: id, change: ch}, true, nil
				}
			}
		}
		if next == nil {
			return modification{}, false, nil
		}
		cur = *next
	}
}

// blob reads a blob's content.
func (r *Repo) blob(ctx context.Context, id hash.ID) ([]byte, error) {
	stored, err := r.objs.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading blob %s: %w", id, err)
	}
	kind, body, err := object.Decode(stored)
	if err != nil {
		return nil, err
	}
	if kind != object.KindBlob {
		return nil, fmt.Errorf("object %s is a %v, not a blob", id, kind)
	}
	return body, nil
}
