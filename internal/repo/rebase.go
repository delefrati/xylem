package repo

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/treeops"
)

// MaxStack is the largest stack Rebase and Land accept (06 §Stack discovery).
const MaxStack = 1000

// ConflictKind classifies a file-level conflict (07 §Rebase).
type ConflictKind int

// Conflict kinds, matching xylem.v1.Conflict.Kind.
const (
	ConflictContent       ConflictKind = 1 // both sides changed the content differently
	ConflictDeleteModify  ConflictKind = 2
	ConflictAddAdd        ConflictKind = 3
	ConflictFileDirectory ConflictKind = 4
	ConflictMode          ConflictKind = 5
)

func (k ConflictKind) String() string {
	switch k {
	case ConflictContent:
		return "content"
	case ConflictDeleteModify:
		return "delete/modify"
	case ConflictAddAdd:
		return "add/add"
	case ConflictFileDirectory:
		return "file/directory"
	case ConflictMode:
		return "mode"
	}
	return fmt.Sprintf("ConflictKind(%d)", int(k))
}

// Conflict is one conflicting path. Ours is the onto side and Theirs the
// stack change's result; a zero id means the side has no file there.
type Conflict struct {
	Path               []byte
	Kind               ConflictKind
	Base, Ours, Theirs hash.ID
}

// RebaseOptions identify the rebase attempt.
type RebaseOptions struct {
	// Committer of rebased changesets ("Name <email>"); empty means each
	// entry's author (the MVP has no caller identity, TODO(M9)).
	Committer []byte
	// Time is the attempt's timestamp, shared by every changeset it creates
	// (default Now).
	Time int64
}

// RebaseResult is the outcome of RebaseStack. On conflict, Conflicts and
// Conflicting are set and Rebased is empty.
type RebaseResult struct {
	Rebased     []hash.ID // rebased (or already well-based) versions of the non-skipped entries, stack order
	Skipped     []hash.ID // original ids whose changes were all already on onto
	Tip         hash.ID   // the last rebased id, or onto if every entry was skipped
	Conflicts   []Conflict
	Conflicting hash.ID
}

// RebaseStack replays stack (bottom → top, each entry's first parent being
// the previous entry) onto the changeset onto (07 §Rebase), with file-level
// conflicts. Entries already based on their new parent are kept as they are;
// entries whose changes are all present on onto are skipped. Rebased
// changesets carry the extra rebased-from = hex(original id).
func (r *Repo) RebaseStack(ctx context.Context, stack []hash.ID, onto hash.ID, opts RebaseOptions) (*RebaseResult, error) {
	switch {
	case len(stack) == 0:
		return nil, fmt.Errorf("%w: empty stack", ErrInvalid)
	case len(stack) > MaxStack:
		return nil, fmt.Errorf("%w: stack of %d changesets, max %d", ErrInvalid, len(stack), MaxStack)
	}
	if opts.Time == 0 {
		opts.Time = r.opts.Now().Unix()
	}
	found, err := r.Changesets(ctx, append([]hash.ID{onto}, stack...))
	if err != nil {
		return nil, err
	}
	if _, ok := found[onto]; !ok {
		return nil, fmt.Errorf("%w: onto changeset %s", ErrNotFound, onto)
	}
	for i, id := range stack {
		x, ok := found[id]
		if !ok {
			return nil, fmt.Errorf("%w: stack changeset %s", ErrNotFound, id)
		}
		if len(x.Parents) != 1 {
			return nil, fmt.Errorf("%w: stack changeset %s has %d parents (stacks cannot contain merges or roots)", ErrInvalid, id.Short(12), len(x.Parents))
		}
		if i > 0 && x.Parents[0] != stack[i-1] {
			return nil, fmt.Errorf("%w: stack changeset %s is not a child of %s", ErrInvalid, id.Short(12), stack[i-1].Short(12))
		}
	}

	res := &RebaseResult{}
	newParent, newTree := onto, found[onto].Core.RootTree
	for _, id := range stack {
		x := found[id]
		base := x.Parents[0]
		if base == newParent { // already based correctly
			res.Rebased = append(res.Rebased, id)
			newParent, newTree = id, x.Core.RootTree
			continue
		}
		baseRow, err := r.source.Core(ctx, []hash.ID{base})
		if err != nil {
			return nil, err
		}
		changes, conflicts, err := r.rebaseChanges(ctx, x.Changes, baseRow[base].RootTree, newTree)
		if err != nil {
			return nil, err
		}
		if len(conflicts) > 0 {
			return &RebaseResult{Conflicts: conflicts, Conflicting: id}, nil
		}
		if len(changes) == 0 { // everything already applied
			res.Skipped = append(res.Skipped, id)
			continue
		}
		rebased, err := r.createRebased(ctx, x, newParent, changes, opts)
		var clash *treeops.FileDirClashError
		if errors.As(err, &clash) {
			return &RebaseResult{Conflicting: id, Conflicts: []Conflict{{Path: clash.Path, Kind: ConflictFileDirectory}}}, nil
		}
		if err != nil {
			return nil, err
		}
		res.Rebased = append(res.Rebased, rebased.ID)
		newParent, newTree = rebased.ID, rebased.Core.RootTree
	}
	res.Tip = newParent
	return res, nil
}

func (r *Repo) createRebased(ctx context.Context, x *Changeset, parent hash.ID, changes []changeset.Change, opts RebaseOptions) (*Changeset, error) {
	extras := maps.Clone(x.Extras)
	if extras == nil {
		extras = map[string][]byte{}
	}
	// The rebased changeset is no longer the imported git commit (git_sha is
	// unique per repo).
	delete(extras, changeset.ExtraGitSHA)
	extras[changeset.ExtraRebasedFrom] = []byte(x.ID.String())
	committer := opts.Committer
	if len(committer) == 0 {
		committer = x.Author.NameEmail
	}
	cs, _, err := r.CreateChangeset(ctx, CreateRequest{
		Parents: []hash.ID{parent}, Author: x.Author, Message: x.Message, Extras: extras, Changes: changes,
	}, CreateOptions{
		Committer:  &changeset.Signature{NameEmail: committer, Time: opts.Time},
		TrustSizes: true, // sizes come from validated changesets and trees
	})
	return cs, err
}

// rebaseChanges resolves each change against the base and onto trees.
func (r *Repo) rebaseChanges(ctx context.Context, changes []changeset.Change, baseTree, ontoTree hash.ID) ([]changeset.Change, []Conflict, error) {
	var out []changeset.Change
	var conflicts []Conflict
	for _, ch := range changes {
		b, err := r.entry(ctx, baseTree, ch.Path)
		if err != nil {
			return nil, nil, err
		}
		o, err := r.entry(ctx, ontoTree, ch.Path)
		if err != nil {
			return nil, nil, err
		}
		res, kind := ResolveChange(b, o, ch)
		switch {
		case kind != 0:
			c := Conflict{Path: ch.Path, Kind: kind}
			if b != nil && b.Mode != object.ModeTree {
				c.Base = b.ID
			}
			if o != nil && o.Mode != object.ModeTree {
				c.Ours = o.ID
			}
			if ch.Op == changeset.OpPut {
				c.Theirs = ch.Blob
			}
			conflicts = append(conflicts, c)
		case res != nil:
			out = append(out, *res)
		}
	}
	return out, conflicts, nil
}

// entry returns the entry at path, or nil if there is none.
func (r *Repo) entry(ctx context.Context, root hash.ID, path []byte) (*object.Entry, error) {
	e, ok, err := treeops.Lookup(ctx, r.trees, root, path)
	if err != nil || !ok {
		return nil, err
	}
	return &e, nil
}

// ResolveChange applies 07's per-path rebase rules to one change of a stack
// entry. b and o are the entries at the path in the entry's base and in onto
// (nil if absent; a ModeTree entry is a directory). It returns the change to
// apply on onto (nil to drop it: onto already has the result) or a conflict
// kind. Mode and content are resolved independently: a side that did not
// change one of them takes the other side's.
func ResolveChange(b, o *object.Entry, ch changeset.Change) (*changeset.Change, ConflictKind) {
	var x *object.Entry
	if ch.Op == changeset.OpPut {
		x = &object.Entry{Mode: ch.Mode, ID: ch.Blob, Size: ch.Size}
	}
	same := func(p, q *object.Entry) bool {
		return p == nil && q == nil || p != nil && q != nil && p.Mode == q.Mode && p.ID == q.ID
	}
	switch {
	case same(b, o): // untouched on onto since base
		return &ch, 0
	case same(o, x): // onto already has the result
		return nil, 0
	case o != nil && o.Mode == object.ModeTree: // onto has a directory here
		return nil, ConflictFileDirectory
	}
	if b != nil && b.Mode == object.ModeTree {
		b = nil // the entry replaced a directory: compare as an add
	}
	switch {
	case b == nil: // both sides added p, differently (o ≠ x; o is a file since b ≠ o)
		return nil, ConflictAddAdd
	case o == nil || x == nil: // one side deleted p, the other modified it
		return nil, ConflictDeleteModify
	}
	// Both sides changed a file: merge mode and content independently.
	mode, modeOK := pick(b.Mode, o.Mode, x.Mode)
	blob, contentOK := pick(b.ID, o.ID, x.ID)
	switch {
	case !contentOK:
		return nil, ConflictContent
	case !modeOK:
		return nil, ConflictMode
	}
	size := x.Size
	if blob == o.ID {
		size = o.Size
	}
	if mode == o.Mode && blob == o.ID {
		return nil, 0 // onto already has the merged result
	}
	return &changeset.Change{Path: ch.Path, Op: changeset.OpPut, Mode: mode, Blob: blob, Size: size}, 0
}

// pick merges one attribute changed from base on two sides.
func pick[T comparable](base, ours, theirs T) (T, bool) {
	switch {
	case ours == base:
		return theirs, true
	case theirs == base, ours == theirs:
		return ours, true
	}
	return base, false
}
