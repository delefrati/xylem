package repo

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/derived"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/treeops"
)

// MainRef is the ref whose first-parent chain is indexed in the mainline table.
// In the MVP it is the only such ref (03 §mainline).
const MainRef = "main"

// ErrNotFastForward reports a non-forced ref update whose new value does not
// have the old one on its first-parent chain.
var ErrNotFastForward = errors.New("not a fast-forward")

var refName = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)

// Ref is a named pointer to a changeset.
type Ref = meta.Ref

// GetRef returns the ref called name.
func (r *Repo) GetRef(ctx context.Context, name string) (Ref, error) {
	ref, err := r.meta.GetRef(ctx, r.id, name)
	if errors.Is(err, meta.ErrNotFound) {
		return Ref{}, fmt.Errorf("ref %q: %w", name, ErrNotFound)
	}
	return ref, err
}

// ListRefs returns the refs whose names start with prefix, by name.
func (r *Repo) ListRefs(ctx context.Context, prefix string) ([]Ref, error) {
	return r.meta.ListRefs(ctx, r.id, prefix)
}

// UpdateRef moves ref name from expected to target (07 §UpdateRef). A zero
// expected means the ref must not exist yet. Without force the update must be
// a fast-forward along the first-parent chain; with force, the mainline is
// rewound to the deepest depth where the old and new chains agree. Updates of
// MainRef maintain the mainline in the same transaction as the ref change.
func (r *Repo) UpdateRef(ctx context.Context, name string, expected, target hash.ID, force bool, actor, reason string) (Ref, error) {
	if !refName.MatchString(name) {
		return Ref{}, fmt.Errorf("%w: ref name %q must match %s", ErrInvalid, name, refName)
	}
	rows, err := r.source.Core(ctx, []hash.ID{target})
	if err != nil {
		return Ref{}, err
	}
	newRow, ok := rows[target]
	if !ok {
		return Ref{}, fmt.Errorf("%w: changeset %s", ErrNotFound, target)
	}
	log := meta.RefLogEntry{Actor: actor, Reason: reason}
	tracked := name == MainRef

	if expected.IsZero() {
		var mainline []meta.MainlineRow
		if tracked {
			if mainline, err = r.firstParentRows(ctx, newRow, -1); err != nil {
				return Ref{}, err
			}
		}
		ref, err := r.meta.CreateRef(ctx, r.id, name, target, mainline, log)
		if errors.Is(err, meta.ErrExists) {
			return Ref{}, fmt.Errorf("%w: ref %q already exists", ErrConflict, name)
		}
		return ref, err
	}

	ff, err := derived.IsFirstParentAncestor(ctx, r.source, expected, target)
	if err != nil {
		if errors.Is(err, derived.ErrMissing) {
			return Ref{}, fmt.Errorf("%w: %w", ErrNotFound, err)
		}
		return Ref{}, err
	}
	if !ff && !force {
		return Ref{}, fmt.Errorf("%w: %s is not on the first-parent chain of %s", ErrNotFastForward, expected.Short(12), target.Short(12))
	}

	var update meta.MainlineUpdate
	if tracked {
		var keep int64
		if ff {
			oldRows, err := r.source.Core(ctx, []hash.ID{expected})
			if err != nil {
				return Ref{}, err
			}
			keep = oldRows[expected].FPDepth
		} else {
			if keep, err = r.agreementDepth(ctx, expected, target); err != nil {
				return Ref{}, err
			}
			update.Rewind, update.KeepThrough = true, keep
		}
		if update.Rows, err = r.firstParentRows(ctx, newRow, keep); err != nil {
			return Ref{}, err
		}
	}
	ref, err := r.meta.CASRef(ctx, r.id, name, expected, target, update, log)
	switch {
	case errors.Is(err, meta.ErrCASMismatch):
		return Ref{}, fmt.Errorf("%w: %w", ErrConflict, err)
	case errors.Is(err, meta.ErrNotFound):
		return Ref{}, fmt.Errorf("%w: ref %q", ErrNotFound, name)
	}
	return ref, err
}

// firstParentRows returns mainline rows for tip's first-parent chain at depths
// above keep, walking fp_parent from tip.
func (r *Repo) firstParentRows(ctx context.Context, tip meta.CoreRow, keep int64) ([]meta.MainlineRow, error) {
	n := tip.FPDepth - keep
	rows := make([]meta.MainlineRow, 0, max(n, 0))
	cur := tip
	for cur.FPDepth > keep {
		rows = append(rows, meta.MainlineRow{Depth: cur.FPDepth, ID: cur.ChangesetID})
		if cur.FPDepth == 0 {
			break
		}
		parent, err := r.source.Core(ctx, []hash.ID{cur.FPParent})
		if err != nil {
			return nil, err
		}
		next, ok := parent[cur.FPParent]
		if !ok {
			return nil, fmt.Errorf("first parent %s: %w", cur.FPParent, ErrNotFound)
		}
		cur = next
	}
	return rows, nil
}

// agreementDepth returns the largest depth d at which the first-parent chains
// of a and b share a changeset, or -1 if they share none. Agreement is
// monotone in d, so it binary-searches.
func (r *Repo) agreementDepth(ctx context.Context, a, b hash.ID) (int64, error) {
	rows, err := r.source.Core(ctx, []hash.ID{a, b})
	if err != nil {
		return 0, err
	}
	agree := func(d int64) (bool, error) {
		x, err := derived.AncestorAtDepth(ctx, r.source, a, d)
		if err != nil {
			return false, err
		}
		y, err := derived.AncestorAtDepth(ctx, r.source, b, d)
		return x == y, err
	}
	lo, hi := int64(-1), min(rows[a].FPDepth, rows[b].FPDepth) // invariant: agree(lo) (or lo = -1)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		ok, err := agree(mid)
		if err != nil {
			return 0, err
		}
		if ok {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo, nil
}

// InitAuthor signs the empty root changeset of a new repository.
var InitAuthor = changeset.Signature{NameEmail: []byte("Xylem <xylem@localhost>")}

// Init creates repository name with an empty root changeset and points
// MainRef at it (xylemd admin init-repo).
func Init(ctx context.Context, m meta.Store, objs store.ObjectStore, name string, opts Options) (*Repo, *Changeset, error) {
	if _, err := m.CreateRepo(ctx, name); err != nil {
		if errors.Is(err, meta.ErrExists) {
			return nil, nil, fmt.Errorf("%w: repo %q already exists", ErrConflict, name)
		}
		return nil, nil, err
	}
	r, err := Open(ctx, m, objs, name, opts)
	if err != nil {
		return nil, nil, err
	}
	author := InitAuthor
	author.Time = r.opts.Now().Unix()
	root, _, err := r.CreateChangeset(ctx, CreateRequest{Author: author, Message: []byte("Initialize repository\n")},
		CreateOptions{AllowRoot: true, AllowEmpty: true})
	if err != nil {
		return nil, nil, err
	}
	if _, err := r.UpdateRef(ctx, MainRef, hash.ID{}, root.ID, false, "admin", "admin"); err != nil {
		return nil, nil, err
	}
	return r, root, nil
}

// ensureEmptyTree stores the empty tree object, which treeops serves without
// a store read but clients fetch like any other tree.
func (r *Repo) ensureEmptyTree(ctx context.Context) error {
	id, stored := object.Encode(object.KindTree, []byte{0})
	if id != treeops.EmptyTree {
		panic("empty tree id mismatch")
	}
	return r.objs.Put(ctx, id, stored)
}

// MaxCandidates is how many matches an ambiguous prefix error lists.
const MaxCandidates = 5

// ResolveHex resolves a full hex id or a unique prefix of at least 8 hex
// characters to a changeset id. An ambiguous prefix fails with ErrInvalid
// listing up to MaxCandidates matches.
func (r *Repo) ResolveHex(ctx context.Context, s string) (hash.ID, error) {
	lo, hi, err := hash.ParsePrefix(s)
	if err != nil {
		return hash.ID{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	ids, err := r.meta.ResolvePrefix(ctx, r.id, lo, hi, MaxCandidates)
	if err != nil {
		return hash.ID{}, err
	}
	return pickOne(s, ids, func(id hash.ID) string { return id.Short(16) })
}

// ResolveGitSHA resolves an imported changeset by its git sha or a unique
// prefix of it (hex, lowercase).
func (r *Repo) ResolveGitSHA(ctx context.Context, prefix string) (hash.ID, error) {
	lo, hi, err := gitPrefixRange(prefix)
	if err != nil {
		return hash.ID{}, err
	}
	matches, err := r.meta.ResolveGitPrefix(ctx, r.id, lo, hi, MaxCandidates)
	if err != nil {
		return hash.ID{}, err
	}
	ids := make([]hash.ID, len(matches))
	names := map[hash.ID]string{}
	for i, m := range matches {
		ids[i] = m.ID
		names[m.ID] = fmt.Sprintf("git:%x", m.GitSHA)
	}
	return pickOne("git:"+prefix, ids, func(id hash.ID) string { return names[id] })
}

func pickOne(what string, ids []hash.ID, name func(hash.ID) string) (hash.ID, error) {
	switch len(ids) {
	case 0:
		return hash.ID{}, fmt.Errorf("%w: no changeset %s", ErrNotFound, what)
	case 1:
		return ids[0], nil
	default:
		var list []string
		for _, id := range ids {
			list = append(list, name(id))
		}
		more := ""
		if len(ids) == MaxCandidates {
			more = ", …"
		}
		return hash.ID{}, fmt.Errorf("%w: %s is ambiguous: %s%s", ErrInvalid, what, strings.Join(list, ", "), more)
	}
}

// gitPrefixRange converts a hex git sha prefix (any length up to 40) into a
// byte range, like hash.ParsePrefix but for 20-byte shas.
func gitPrefixRange(prefix string) (lo, hi []byte, err error) {
	if len(prefix) == 0 || len(prefix) > 40 {
		return nil, nil, fmt.Errorf("%w: git sha prefix %q", ErrInvalid, prefix)
	}
	padded := prefix
	if len(padded)%2 == 1 {
		padded += "0"
	}
	lo, err = hex.DecodeString(padded)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: git sha prefix %q", ErrInvalid, prefix)
	}
	hi = bytes.Clone(lo)
	step := byte(1)
	if len(prefix)%2 == 1 {
		step = 0x10
	}
	for i := len(hi) - 1; i >= 0; i-- {
		carry := hi[i] > 0xff-step
		hi[i] += step
		if !carry {
			return lo, hi, nil
		}
		step = 1
	}
	return lo, nil, nil
}
