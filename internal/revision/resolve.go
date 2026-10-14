package revision

import (
	"context"
	"errors"
	"fmt"

	"github.com/delefrati/xylem/internal/derived"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
)

// Resolve resolves a revision string to a changeset id in r. Syntax errors
// and ambiguous prefixes wrap repo.ErrInvalid; unknown refs, ids and
// out-of-range ancestors wrap repo.ErrNotFound.
func Resolve(ctx context.Context, r *repo.Repo, s string) (hash.ID, error) {
	rev, err := Parse(s)
	if err != nil {
		return hash.ID{}, fmt.Errorf("%w: %w", repo.ErrInvalid, err)
	}
	return ResolveParsed(ctx, r, rev)
}

// ResolveParsed resolves an already parsed revision.
func ResolveParsed(ctx context.Context, r *repo.Repo, rev *Rev) (hash.ID, error) {
	id, err := resolveBase(ctx, r, rev)
	if err != nil {
		return hash.ID{}, err
	}
	for _, op := range rev.Ops {
		if id, err = apply(ctx, r, id, op); err != nil {
			return hash.ID{}, fmt.Errorf("%s: %w", rev, err)
		}
	}
	return id, nil
}

func resolveBase(ctx context.Context, r *repo.Repo, rev *Rev) (hash.ID, error) {
	switch rev.Kind {
	case At:
		return hash.ID{}, fmt.Errorf("%w: @ (the workspace parent) is only meaningful in a workspace", repo.ErrInvalid)
	case GitSHA:
		return r.ResolveGitSHA(ctx, rev.Base)
	}
	// Refs win over hex prefixes, as in git.
	ref, err := r.GetRef(ctx, rev.Base)
	if err == nil {
		return ref.ID, nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return hash.ID{}, err
	}
	if IsHex(rev.Base) {
		return r.ResolveHex(ctx, rev.Base)
	}
	return hash.ID{}, fmt.Errorf("%w: unknown revision %q", repo.ErrNotFound, rev.Base)
}

func apply(ctx context.Context, r *repo.Repo, id hash.ID, op Op) (hash.ID, error) {
	src := r.Source()
	switch op.Kind {
	case Tilde:
		if op.N == 0 {
			return id, nil
		}
		rows, err := src.Core(ctx, []hash.ID{id})
		if err != nil {
			return hash.ID{}, err
		}
		depth := rows[id].FPDepth
		if int64(op.N) > depth {
			return hash.ID{}, fmt.Errorf("%w: ~%d goes past the root (%s has %d first-parent ancestors)", repo.ErrNotFound, op.N, id.Short(12), depth)
		}
		return derived.AncestorAtDepth(ctx, src, id, depth-int64(op.N))
	default: // Caret
		if op.N == 0 {
			return id, nil
		}
		parents, err := src.Parents(ctx, []hash.ID{id})
		if err != nil {
			return hash.ID{}, err
		}
		ps := parents[id]
		if op.N > len(ps) {
			return hash.ID{}, fmt.Errorf("%w: ^%d but %s has %d parents", repo.ErrNotFound, op.N, id.Short(12), len(ps))
		}
		return ps[op.N-1], nil
	}
}
