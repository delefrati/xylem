package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/delefrati/xylem/internal/paths"
)

// ErrFullCheckout reports a sparse change on a full checkout, which already
// has every directory.
var ErrFullCheckout = errors.New("the workspace is a full checkout (it has every directory)")

// SparseAdd returns the profile with dirs added. A dir already covered by an
// entry is skipped; entries under an added dir are replaced by it.
func SparseAdd(profile, dirs []Path) ([]Path, error) {
	if len(profile) == 0 {
		return nil, Usage(ErrFullCheckout)
	}
	out := slices.Clone(profile)
	for _, d := range dirs {
		if slices.ContainsFunc(out, func(e Path) bool { return paths.IsUnder(d, e) }) {
			continue
		}
		out = slices.DeleteFunc(out, func(e Path) bool { return paths.IsUnder(e, d) })
		out = append(out, d)
	}
	return out, nil
}

// SparseRemove returns the profile without dirs, each of which must be an
// entry. The last entry cannot be removed: an empty profile means a full
// checkout.
func SparseRemove(profile, dirs []Path) ([]Path, error) {
	if len(profile) == 0 {
		return nil, Usage(ErrFullCheckout)
	}
	out := slices.Clone(profile)
	for _, d := range dirs {
		i := slices.IndexFunc(out, func(e Path) bool { return bytes.Equal(e, d) })
		if i < 0 {
			return nil, Usage(fmt.Errorf("%q is not in the sparse profile", d))
		}
		out = slices.Delete(out, i, i+1)
	}
	if len(out) == 0 {
		return nil, Usage(errors.New("cannot remove every directory from the sparse profile (root files are always checked out; an empty profile would mean everything)"))
	}
	return out, nil
}

// SetSparse applies a new sparse profile at the current parent: newly in-scope
// files are fetched and written, out-of-scope ones deleted. It refuses (exit
// 4) if a file it would delete or overwrite has local changes. The caller
// holds the workspace lock.
func (w *Workspace) SetSparse(ctx context.Context, f *Fetcher, profile []Path) (*CheckoutResult, error) {
	if w.State.Parent.IsZero() {
		return nil, errors.New("the workspace has no checkout yet")
	}
	return w.Checkout(ctx, f, Target{Changeset: w.State.Parent, Root: w.State.RootTree, Sparse: profile}, false)
}
