package repo

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/derived"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/treeops"
)

// VerifyOptions selects what Verify checks.
type VerifyOptions struct {
	// Sample, if > 0, checks that many random changesets instead of all.
	Sample int
	// Objects also checks that every blob and tree a changeset introduces exists.
	Objects bool
	// MaxViolations stops collecting after this many (default 1000).
	MaxViolations int
	// Progress, if set, is called after each batch with the count so far.
	Progress func(checked int)
}

// Violation is one broken invariant.
type Violation struct {
	Changeset hash.ID // zero for ref/mainline violations
	Check     string
	Detail    string
}

func (v Violation) String() string {
	if v.Changeset.IsZero() {
		return v.Check + ": " + v.Detail
	}
	return v.Changeset.Short(12) + " " + v.Check + ": " + v.Detail
}

// VerifyReport summarizes a Verify run.
type VerifyReport struct {
	Changesets int
	Refs       int
	Violations []Violation
	Truncated  bool // more violations than MaxViolations
}

const verifyBatch = 500

// Verify recomputes and cross-checks stored data (xylemd verify, M2.8):
//   - body: hashes to the id, decodes, agrees with the denormalized columns and
//     changeset_files;
//   - root_tree: equals applying the changes to the first parent's root tree;
//   - core: gen, fp_depth, fp_parent and fp_jump equal their recomputation;
//   - seq(child) > seq(parent);
//   - mainline of MainRef equals the first-parent walk from its tip;
//   - with Objects: every PUT blob, the root tree, and each tree on a changed
//     path exist.
//
// It does not write anything.
func (r *Repo) Verify(ctx context.Context, opts VerifyOptions) (*VerifyReport, error) {
	if opts.MaxViolations == 0 {
		opts.MaxViolations = 1000
	}
	v := &verifier{r: r, opts: opts, report: &VerifyReport{}, seqs: map[hash.ID]int64{}}

	if opts.Sample > 0 {
		rows, err := r.meta.SampleChangesets(ctx, r.id, opts.Sample)
		if err != nil {
			return nil, err
		}
		if err := v.batch(ctx, rows); err != nil {
			return nil, err
		}
	} else {
		after := int64(math.MinInt64)
		for {
			rows, err := r.meta.ListChangesets(ctx, r.id, after, verifyBatch)
			if err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				break
			}
			if err := v.batch(ctx, rows); err != nil {
				return nil, err
			}
			after = rows[len(rows)-1].Seq
		}
	}
	if err := v.refs(ctx); err != nil {
		return nil, err
	}
	return v.report, nil
}

type verifier struct {
	r      *Repo
	opts   VerifyOptions
	report *VerifyReport
	seqs   map[hash.ID]int64 // seq of changesets seen so far (full mode)
}

func (v *verifier) fail(id hash.ID, check, format string, args ...any) {
	if len(v.report.Violations) >= v.opts.MaxViolations {
		v.report.Truncated = true
		return
	}
	v.report.Violations = append(v.report.Violations, Violation{Changeset: id, Check: check, Detail: fmt.Sprintf(format, args...)})
}

func (v *verifier) batch(ctx context.Context, rows []meta.ChangesetRow) error {
	ids := make([]hash.ID, len(rows))
	var parents []hash.ID
	for i, row := range rows {
		ids[i] = row.ID
		v.seqs[row.ID] = row.Seq
		parents = append(parents, row.Parents...)
	}
	cores, err := v.r.meta.GetCore(ctx, v.r.id, ids)
	if err != nil {
		return err
	}
	files, err := v.r.meta.FilesFor(ctx, v.r.id, ids)
	if err != nil {
		return err
	}
	// Parent seqs not seen yet (sample mode, or out-of-order inserts).
	var unknown []hash.ID
	for _, p := range parents {
		if _, ok := v.seqs[p]; !ok && !slices.Contains(unknown, p) {
			unknown = append(unknown, p)
		}
	}
	if len(unknown) > 0 {
		got, err := v.r.meta.GetChangesets(ctx, v.r.id, unknown)
		if err != nil {
			return err
		}
		for id, row := range got {
			v.seqs[id] = row.Seq
		}
	}
	for _, row := range rows {
		if err := v.changeset(ctx, row, cores, files[row.ID]); err != nil {
			return err
		}
	}
	v.report.Changesets += len(rows)
	if v.opts.Progress != nil {
		v.opts.Progress(v.report.Changesets)
	}
	return nil
}

func (v *verifier) changeset(ctx context.Context, row meta.ChangesetRow, cores map[hash.ID]meta.CoreRow, files []meta.FileRow) error {
	id := row.ID
	if got := object.ID(object.KindChangeset, row.Body); got != id {
		v.fail(id, "body", "body hashes to %s", got.Short(12))
	}
	cs, err := changeset.Decode(row.Body)
	if err != nil {
		v.fail(id, "body", "%v", err)
		return nil
	}
	v.columns(row, cs, files)

	for _, p := range cs.Parents {
		ps, ok := v.seqs[p]
		switch {
		case !ok:
			v.fail(id, "parents", "parent %s does not exist", p.Short(12))
		case ps >= row.Seq:
			v.fail(id, "seq", "seq %d is not greater than parent %s seq %d", row.Seq, p.Short(12), ps)
		}
	}

	core, ok := cores[id]
	if !ok {
		v.fail(id, "core", "no core_derived row")
		return nil
	}
	base := treeops.EmptyTree
	if len(cs.Parents) > 0 {
		prow, err := v.r.source.Core(ctx, cs.Parents[:1])
		if err != nil {
			return err
		}
		p, ok := prow[cs.Parents[0]]
		if !ok {
			v.fail(id, "core", "first parent has no core row")
			return nil
		}
		base = p.RootTree
	}
	root, _, err := treeops.Apply(ctx, v.r.trees, base, cs.Changes)
	switch {
	case err != nil:
		v.fail(id, "root_tree", "re-applying changes: %v", err)
	case root != core.RootTree:
		v.fail(id, "root_tree", "stored %s, recomputed %s", core.RootTree.Short(12), root.Short(12))
	}
	want, err := derived.ComputeCore(ctx, v.r.source, id, cs.Parents, core.RootTree)
	if err != nil {
		v.fail(id, "core", "recomputing: %v", err)
	} else if want != core {
		v.fail(id, "core", "stored %+v, recomputed %+v", core, want)
	}
	if v.opts.Objects {
		return v.objects(ctx, id, core.RootTree, cs.Changes)
	}
	return nil
}

// columns checks the denormalized columns and file rows against the body.
func (v *verifier) columns(row meta.ChangesetRow, cs *changeset.Changeset, files []meta.FileRow) {
	id := row.ID
	if !slices.Equal(row.Parents, cs.Parents) {
		v.fail(id, "columns", "parents column differs from body")
	}
	if !bytes.Equal(row.Author, cs.Author.NameEmail) || !bytes.Equal(row.Committer, cs.Committer.NameEmail) {
		v.fail(id, "columns", "author or committer column differs from body")
	}
	if row.Summary != cs.Summary() {
		v.fail(id, "columns", "summary %q, body gives %q", row.Summary, cs.Summary())
	}
	if want, err := gitSHAExtra(cs.Extras); err != nil || !bytes.Equal(row.GitSHA, want) {
		v.fail(id, "columns", "git_sha column differs from the git-sha extra")
	}
	if len(files) != len(cs.Changes) {
		v.fail(id, "files", "%d file rows, body has %d changes", len(files), len(cs.Changes))
		return
	}
	for i, ch := range cs.Changes {
		f := files[i]
		if !bytes.Equal(f.Path, ch.Path) || f.Op != ch.Op || f.Mode != ch.Mode || f.Size != ch.Size || f.Blob != ch.Blob {
			v.fail(id, "files", "file row %q differs from the body's change %q", f.Path, ch.Path)
			return
		}
	}
}

// objects checks the blobs and trees introduced by a changeset.
func (v *verifier) objects(ctx context.Context, id, root hash.ID, changes []changeset.Change) error {
	var blobs []hash.ID
	for _, ch := range changes {
		if ch.Op == changeset.OpPut {
			blobs = append(blobs, ch.Blob)
		}
	}
	want := append([]hash.ID{root}, blobs...)
	present, err := v.r.objs.Has(ctx, want)
	if err != nil {
		return err
	}
	for i, ok := range present {
		if !ok {
			v.fail(id, "objects", "missing object %s", want[i].Short(12))
		}
	}
	if !present[0] && root != treeops.EmptyTree {
		return nil
	}
	// Looking up each changed path reads every tree down to its directory
	// (a deleted path's directory may be gone; Lookup then stops early).
	for _, ch := range changes {
		if _, _, err := treeops.Lookup(ctx, v.r.trees, root, ch.Path); err != nil {
			v.fail(id, "objects", "tree on path %q: %v", ch.Path, err)
		}
	}
	return nil
}

// refs checks each ref's target and the mainline of MainRef.
func (v *verifier) refs(ctx context.Context) error {
	refs, err := v.r.meta.ListRefs(ctx, v.r.id, "")
	if err != nil {
		return err
	}
	v.report.Refs = len(refs)
	for _, ref := range refs {
		rows, err := v.r.source.Core(ctx, []hash.ID{ref.ID})
		if err != nil {
			return err
		}
		tip, ok := rows[ref.ID]
		if !ok {
			v.fail(hash.ID{}, "refs", "ref %s points at missing changeset %s", ref.Name, ref.ID.Short(12))
			continue
		}
		if ref.Name == MainRef {
			if err := v.mainline(ctx, tip); err != nil {
				return err
			}
		}
	}
	return nil
}

// mainline compares MainRef's mainline rows, deepest first, with the
// first-parent walk from its tip.
func (v *verifier) mainline(ctx context.Context, tip meta.CoreRow) error {
	cur := tip
	maxDepth := int64(math.MaxInt64)
	for {
		rows, err := v.r.meta.MainlineRange(ctx, v.r.id, MainRef, maxDepth, verifyBatch)
		if err != nil {
			return err
		}
		if maxDepth == math.MaxInt64 && (len(rows) == 0 || rows[0].Depth != tip.FPDepth) {
			v.fail(hash.ID{}, "mainline", "deepest row does not match the tip depth %d", tip.FPDepth)
			return nil
		}
		for _, row := range rows {
			if row.Depth != cur.FPDepth || row.ID != cur.ChangesetID {
				v.fail(hash.ID{}, "mainline", "row at depth %d is %s, first-parent walk gives %s at depth %d",
					row.Depth, row.ID.Short(12), cur.ChangesetID.Short(12), cur.FPDepth)
				return nil
			}
			if cur.FPDepth == 0 {
				return nil
			}
			next, err := v.r.source.Core(ctx, []hash.ID{cur.FPParent})
			if err != nil {
				return err
			}
			cur = next[cur.FPParent]
		}
		if len(rows) < verifyBatch {
			v.fail(hash.ID{}, "mainline", "rows end at depth %d before reaching the root", cur.FPDepth)
			return nil
		}
		maxDepth = rows[len(rows)-1].Depth - 1
	}
}
