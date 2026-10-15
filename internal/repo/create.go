package repo

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/derived"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/treeops"
)

// Changeset is a stored changeset with its core derived data.
type Changeset struct {
	changeset.Changeset
	ID   hash.ID
	Seq  int64
	Core meta.CoreRow
}

// CreateRequest describes a changeset to create. Changes may be in any order.
type CreateRequest struct {
	Parents []hash.ID
	Author  changeset.Signature
	Message []byte
	Extras  map[string][]byte
	Changes []changeset.Change
}

// CreateOptions adjusts CreateChangeset for trusted callers.
type CreateOptions struct {
	AllowRoot  bool                 // allow a root changeset in a non-empty repo (importer, init-repo)
	AllowEmpty bool                 // allow a single-parent changeset with no changes (importer)
	Committer  *changeset.Signature // default: CommitterName, else the author, at Now, tz 0
	// CommitterName is the committer identity ("Name <email>") stamped at Now
	// when Committer is nil: the authenticated caller (M9.1).
	CommitterName []byte
	TrustSizes    bool // skip the blob size check (importer computed them)
}

// MissingBlobsError lists blobs referenced by a changeset that the object
// store does not have (up to 20).
type MissingBlobsError struct {
	IDs   []hash.ID
	Total int
}

func (e *MissingBlobsError) Error() string {
	hexes := make([]string, len(e.IDs))
	for i, id := range e.IDs {
		hexes[i] = id.String()
	}
	return fmt.Sprintf("%d missing blobs: %s", e.Total, strings.Join(hexes, ", "))
}

const maxMissingListed = 20

// CreateChangeset creates a changeset (07 §CreateChangeset). It returns the
// stored changeset and whether it already existed. Concurrent creates of the
// same changeset are safe: one inserts, the others report already existed.
func (r *Repo) CreateChangeset(ctx context.Context, req CreateRequest, opts CreateOptions) (*Changeset, bool, error) {
	// 1–2. Canonicalize and validate shape (Encode sorts copies of changes).
	cs := changeset.Changeset{
		Parents: req.Parents,
		Author:  req.Author,
		Message: req.Message,
		Extras:  req.Extras,
		Changes: req.Changes,
	}
	if err := cs.ValidateShape(changeset.ShapeOptions{AllowEmpty: opts.AllowEmpty}); err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	for _, ch := range cs.Changes {
		if ch.Op == changeset.OpPut && ch.Size > r.opts.MaxBlobSize {
			return nil, false, fmt.Errorf("%w: %q is %d bytes, max %d", ErrInvalid, ch.Path, ch.Size, r.opts.MaxBlobSize)
		}
	}
	gitSHA, err := gitSHAExtra(cs.Extras)
	if err != nil {
		return nil, false, err
	}
	// 4. Load parents (fails fast on unknown parents).
	parentRows, err := r.source.Core(ctx, cs.Parents)
	if err != nil {
		return nil, false, err
	}
	for _, p := range cs.Parents {
		if _, ok := parentRows[p]; !ok {
			return nil, false, fmt.Errorf("%w: parent %s", ErrNotFound, p)
		}
	}
	baseTree := treeops.EmptyTree
	if len(cs.Parents) > 0 {
		baseTree = parentRows[cs.Parents[0]].RootTree
	}

	// 3. Blobs exist and declared sizes match.
	if err := r.checkBlobs(ctx, baseTree, cs.Changes, opts.TrustSizes); err != nil {
		return nil, false, err
	}

	// 5–6. Committer, encode, id.
	switch {
	case opts.Committer != nil:
		cs.Committer = *opts.Committer
	case opts.CommitterName != nil:
		cs.Committer = changeset.Signature{NameEmail: opts.CommitterName, Time: r.opts.Now().Unix()}
	default:
		cs.Committer = changeset.Signature{NameEmail: cs.Author.NameEmail, Time: r.opts.Now().Unix()}
	}
	body, err := cs.Encode()
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	id := object.ID(object.KindChangeset, body)

	// 7. Fast idempotency check.
	if existing, err := r.existing(ctx, id); err != nil || existing != nil {
		return existing, existing != nil, err
	}
	// A new root needs an empty repo (checked after step 7, so retrying the
	// creation of an existing root is idempotent).
	if len(cs.Parents) == 0 && !opts.AllowRoot {
		if err := r.requireEmpty(ctx); err != nil {
			return nil, false, err
		}
	}

	// 8. Apply changes to the first parent's tree; store new trees first.
	root, newTrees, err := treeops.Apply(ctx, r.trees, baseTree, cs.Changes)
	if err != nil {
		if errors.Is(err, treeops.ErrDeleteMissing) || errors.Is(err, treeops.ErrNoopPut) ||
			errors.Is(err, treeops.ErrDuplicatePath) || errors.As(err, new(*treeops.FileDirClashError)) {
			return nil, false, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		return nil, false, err
	}
	for _, nt := range newTrees {
		if err := r.objs.Put(ctx, nt.ID, nt.Stored); err != nil {
			return nil, false, fmt.Errorf("storing tree %s: %w", nt.ID, err)
		}
		r.trees.Add(nt.ID, nt.Tree)
	}
	if root == treeops.EmptyTree {
		if err := r.ensureEmptyTree(ctx); err != nil {
			return nil, false, fmt.Errorf("storing the empty tree: %w", err)
		}
	}

	// 9. Core derived data.
	core, err := derived.ComputeCore(ctx, r.source, id, cs.Parents, root)
	if err != nil {
		return nil, false, err
	}

	// 10. Transaction.
	row := meta.ChangesetRow{
		ID: id, Parents: cs.Parents, Body: body,
		Author: cs.Author.NameEmail, AuthorTime: cs.Author.Time,
		Committer: cs.Committer.NameEmail, CommitterTime: cs.Committer.Time,
		Summary: cs.Summary(), GitSHA: gitSHA,
	}
	files := make([]meta.FileRow, len(cs.Changes))
	for i, ch := range cs.Changes {
		files[i] = meta.FileRow{Path: ch.Path, Op: ch.Op, Mode: ch.Mode, Size: ch.Size, Blob: ch.Blob}
	}
	seq, inserted, err := r.meta.InsertChangeset(ctx, r.id, row, files, core)
	if err != nil {
		return nil, false, err
	}
	r.source.AddCore(core)
	r.source.AddParents(id, cs.Parents)

	// 11. Return the canonical changeset.
	stored, err := changeset.Decode(body)
	if err != nil {
		return nil, false, err
	}
	return &Changeset{Changeset: *stored, ID: id, Seq: seq, Core: core}, !inserted, nil
}

// requireEmpty fails unless the repository has no changesets.
func (r *Repo) requireEmpty(ctx context.Context) error {
	any, err := r.meta.ResolvePrefix(ctx, r.id, []byte{}, nil, 1)
	if err != nil {
		return err
	}
	if len(any) > 0 {
		return fmt.Errorf("%w: root changesets need AllowRoot in a non-empty repository", ErrInvalid)
	}
	return nil
}

// existing returns the stored changeset id if it exists, or nil.
func (r *Repo) existing(ctx context.Context, id hash.ID) (*Changeset, error) {
	cores, err := r.source.Core(ctx, []hash.ID{id})
	if err != nil {
		return nil, err
	}
	core, ok := cores[id]
	if !ok {
		return nil, nil
	}
	rows, err := r.meta.GetChangesets(ctx, r.id, []hash.ID{id})
	if err != nil {
		return nil, err
	}
	stored, err := changeset.Decode(rows[id].Body)
	if err != nil {
		return nil, err
	}
	return &Changeset{Changeset: *stored, ID: id, Seq: rows[id].Seq, Core: core}, nil
}

// gitSHAExtra returns the binary git SHA-1 from the git-sha extra, if present.
func gitSHAExtra(extras map[string][]byte) ([]byte, error) {
	v, ok := extras[changeset.ExtraGitSHA]
	if !ok {
		return nil, nil
	}
	sha, err := hex.DecodeString(string(v))
	if err != nil || len(sha) != 20 || strings.ToLower(string(v)) != string(v) {
		return nil, fmt.Errorf("%w: extra %s must be 40 lowercase hex characters, got %q", ErrInvalid, changeset.ExtraGitSHA, v)
	}
	return sha, nil
}

// checkBlobs implements 03 §Validation rule 3: every PUT blob exists, and for
// blobs newly referenced at a path (not the parent's blob there), the declared
// size matches the blob's header.
func (r *Repo) checkBlobs(ctx context.Context, baseTree hash.ID, changes []changeset.Change, trustSizes bool) error {
	var ids []hash.ID
	sizes := map[hash.ID]uint64{}
	for _, ch := range changes {
		if ch.Op != changeset.OpPut {
			continue
		}
		if prev, ok := sizes[ch.Blob]; ok && prev != ch.Size {
			return fmt.Errorf("%w: blob %s declared with sizes %d and %d", ErrInvalid, ch.Blob.Short(12), prev, ch.Size)
		}
		if _, ok := sizes[ch.Blob]; !ok {
			ids = append(ids, ch.Blob)
		}
		sizes[ch.Blob] = ch.Size
	}
	if len(ids) == 0 {
		return nil
	}
	present, err := r.objs.Has(ctx, ids)
	if err != nil {
		return fmt.Errorf("checking blobs: %w", err)
	}
	missing := &MissingBlobsError{}
	for i, ok := range present {
		if !ok {
			missing.Total++
			if len(missing.IDs) < maxMissingListed {
				missing.IDs = append(missing.IDs, ids[i])
			}
		}
	}
	if missing.Total > 0 {
		return missing
	}
	if trustSizes {
		return nil
	}

	// Newly referenced blobs: those that differ from the parent's entry.
	var check []hash.ID
	seen := map[hash.ID]bool{}
	for _, ch := range changes {
		if ch.Op != changeset.OpPut || seen[ch.Blob] {
			continue
		}
		e, found, err := treeops.Lookup(ctx, r.trees, baseTree, ch.Path)
		if err != nil {
			return err
		}
		if found && e.ID == ch.Blob && e.Mode != object.ModeTree {
			continue // already referenced at this path; size was checked then
		}
		seen[ch.Blob] = true
		check = append(check, ch.Blob)
	}
	slices.SortFunc(check, hash.Compare)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r.opts.SizeCheckLimit)
	for _, id := range check {
		g.Go(func() error {
			stored, err := r.objs.Get(gctx, id)
			if err != nil {
				return fmt.Errorf("reading blob %s: %w", id, err)
			}
			kind, n, err := object.PeekHeader(stored)
			if err != nil {
				return fmt.Errorf("%w: blob %s: %w", ErrInvalid, id, err)
			}
			if kind != object.KindBlob {
				return fmt.Errorf("%w: %s is a %v, not a blob", ErrInvalid, id, kind)
			}
			if n < 0 || uint64(n) != sizes[id] {
				return fmt.Errorf("%w: blob %s is %d bytes, declared %d", ErrInvalid, id, n, sizes[id])
			}
			return nil
		})
	}
	return g.Wait()
}
