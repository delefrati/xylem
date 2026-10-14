package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/store"
)

// This file is the git importer's view of the repository: the git sha map,
// import progress and the git blob table (08-git-import.md).

// GitBlob maps a git blob sha to the Xylem blob with the same content.
type GitBlob = meta.GitBlob

// OpenOrCreate opens repository name, creating it (with no changesets or
// refs) if it does not exist.
func OpenOrCreate(ctx context.Context, m meta.Store, objs store.ObjectStore, name string, opts Options) (*Repo, error) {
	r, err := Open(ctx, m, objs, name, opts)
	if !errors.Is(err, ErrNotFound) {
		return r, err
	}
	if _, err := m.CreateRepo(ctx, name); err != nil && !errors.Is(err, meta.ErrExists) {
		return nil, err
	}
	return Open(ctx, m, objs, name, opts)
}

// GitSHAMap returns git commit sha → changeset id for all imported changesets.
func (r *Repo) GitSHAMap(ctx context.Context) (map[[20]byte]hash.ID, error) {
	return r.meta.GitSHAMap(ctx, r.id)
}

// ImportState returns the last imported git sha for (source, branch);
// ok is false if nothing was imported yet.
func (r *Repo) ImportState(ctx context.Context, source, branch string) (sha [20]byte, ok bool, err error) {
	sha, err = r.meta.GetImportState(ctx, r.id, source, branch)
	if errors.Is(err, meta.ErrNotFound) {
		return sha, false, nil
	}
	return sha, err == nil, err
}

// SetImportState records the last imported git sha for (source, branch).
func (r *Repo) SetImportState(ctx context.Context, source, branch string, sha [20]byte) error {
	return r.meta.PutImportState(ctx, r.id, source, branch, sha)
}

// GitBlobs returns the known git blob mappings among shas.
func (r *Repo) GitBlobs(ctx context.Context, shas [][20]byte) (map[[20]byte]GitBlob, error) {
	return r.meta.GetGitBlobs(ctx, r.id, shas)
}

// RecordGitBlobs stores git blob mappings. Record a blob only after storing it.
func (r *Repo) RecordGitBlobs(ctx context.Context, blobs []GitBlob) error {
	return r.meta.InsertGitBlobs(ctx, r.id, blobs)
}

// Changeset returns a stored changeset with its core data.
func (r *Repo) Changeset(ctx context.Context, id hash.ID) (*Changeset, error) {
	cs, err := r.existing(ctx, id)
	if err != nil {
		return nil, err
	}
	if cs == nil {
		return nil, fmt.Errorf("%w: changeset %s", ErrNotFound, id)
	}
	return cs, nil
}
