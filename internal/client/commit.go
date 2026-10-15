package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
)

// ErrNothingToCommit reports a commit with no changes.
var ErrNothingToCommit = errors.New("nothing to commit (untracked files need xy add, or commit -A)")

// CommitOptions configures Commit.
type CommitOptions struct {
	Message []byte
	All     bool             // also commit untracked (?) files
	Author  string           // "Name <email>"; default ResolveAuthor
	Now     func() time.Time // default time.Now
}

// CommitResult describes a new draft changeset.
type CommitResult struct {
	Changeset hash.ID
	Changes   int
	Uploaded  UploadStats
}

// pending is one change to commit.
type pending struct {
	path  []byte
	put   bool
	mode  object.Mode
	blob  hash.ID
	size  int64
	info  fs.FileInfo // lstat taken before reading the content
	added bool
}

// Commit creates a draft changeset on top of @ from the working copy (06
// §Commit): status, build and upload the blobs the server lacks,
// CreateChangeset, then move @ and update the dirstate. The caller holds the
// workspace lock.
func (w *Workspace) Commit(ctx context.Context, f *Fetcher, opts CommitOptions) (*CommitResult, error) {
	if w.State.Parent.IsZero() {
		return nil, errors.New("the workspace has no parent changeset")
	}
	if len(bytes.TrimSpace(opts.Message)) == 0 {
		return nil, Usage(errors.New("empty commit message (use -m)"))
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	author := opts.Author
	if author == "" {
		var err error
		if author, err = ResolveAuthor(); err != nil {
			return nil, err
		}
	}
	status, err := w.Status(ctx, f)
	if err != nil {
		return nil, err
	}

	var changes []pending
	for _, s := range status {
		switch s.Code {
		case StatusDeleted:
			changes = append(changes, pending{path: s.Path})
		case StatusModified, StatusAdded, StatusUntracked:
			if s.Code == StatusUntracked && !opts.All {
				continue
			}
			p, err := w.stageBlob(f.cache, s.Path)
			if err != nil {
				return nil, err
			}
			p.added = s.Code != StatusModified
			changes = append(changes, p)
		}
	}
	if len(changes) == 0 {
		return nil, ErrNothingToCommit
	}

	var blobs []hash.ID
	req := &xylemv1.CreateChangesetRequest{Repo: f.repo, Parents: [][]byte{w.State.Parent[:]}, Message: opts.Message}
	for _, c := range changes {
		fc := &xylemv1.FileChange{Path: c.path, Op: xylemv1.FileChange_OP_DELETE}
		if c.put {
			fc = &xylemv1.FileChange{Path: c.path, Op: xylemv1.FileChange_OP_PUT, Mode: protoMode(c.mode), Size: uint64(max(c.size, 0)), BlobId: c.blob[:]}
			blobs = append(blobs, c.blob)
		}
		req.Changes = append(req.Changes, fc)
	}
	uploaded, err := f.upload(ctx, blobs)
	if err != nil {
		return nil, fmt.Errorf("uploading blobs: %w", err)
	}
	now := opts.Now()
	_, offset := now.Zone()
	req.Author = &xylemv1.Signature{NameEmail: []byte(author), Time: timestamppb.New(now.Truncate(time.Second)), TzOffsetMinutes: int32(offset / 60)} //nolint:gosec // G115: a zone offset in minutes fits int32
	resp, err := f.conn.Write.CreateChangeset(ctx, req)
	if err != nil {
		return nil, err
	}
	var id, root hash.ID
	copy(id[:], resp.GetChangeset().GetId())
	copy(root[:], resp.GetChangeset().GetRootTree())

	ds, err := LoadDirstate(w.Meta("dirstate"))
	if err != nil {
		return nil, err
	}
	for _, c := range changes {
		if c.put {
			ds.Set(entryFor(c.path, c.info, c.mode, c.blob))
		} else {
			ds.Delete(c.path)
		}
	}
	ds.Root = root
	if err := ds.Save(w.Meta("dirstate")); err != nil {
		return nil, err
	}
	w.State.Parent, w.State.RootTree, w.State.Added = id, root, []Path{}
	if err := w.SaveState(); err != nil { // last, as in checkout
		return nil, err
	}
	return &CommitResult{Changeset: id, Changes: len(changes), Uploaded: uploaded}, f.cache.EnsureLimit()
}

// stageBlob reads a working file into a blob in the local cache. The lstat
// comes first, so a later change to the file shows in its stat.
func (w *Workspace) stageBlob(cache *ObjectCache, path []byte) (pending, error) {
	abs := filepath.Join(w.Root, string(path))
	info, err := os.Lstat(abs)
	if err != nil {
		return pending{}, err
	}
	mode, ok := FileMode(info)
	if !ok {
		return pending{}, fmt.Errorf("%s: not a regular file or symlink", path)
	}
	var content []byte
	if mode == object.ModeSymlink {
		target, err := os.Readlink(abs)
		if err != nil {
			return pending{}, err
		}
		content = []byte(target)
	} else if content, err = os.ReadFile(abs); err != nil {
		return pending{}, err
	}
	id, stored := object.Encode(object.KindBlob, content)
	if !cache.Has(id) {
		if err := cache.Put(id, stored); err != nil {
			return pending{}, err
		}
	}
	return pending{path: path, put: true, mode: mode, blob: id, size: int64(len(content)), info: info}, nil
}

func protoMode(m object.Mode) xylemv1.FileMode {
	switch m {
	case object.ModeExec:
		return xylemv1.FileMode_FILE_MODE_EXEC
	case object.ModeSymlink:
		return xylemv1.FileMode_FILE_MODE_SYMLINK
	}
	return xylemv1.FileMode_FILE_MODE_FILE
}

// Add marks untracked files under paths (repository paths; a directory adds
// every untracked, non-ignored file below it) for the next commit. It
// returns the paths newly added. A path that does not exist is an error.
func (w *Workspace) Add(ctx context.Context, f *Fetcher, paths [][]byte) ([][]byte, error) {
	for _, p := range paths {
		if _, err := os.Lstat(filepath.Join(w.Root, string(p))); err != nil {
			return nil, Usage(fmt.Errorf("%q: %w", p, err))
		}
	}
	status, err := w.Status(ctx, f)
	if err != nil {
		return nil, err
	}
	var added [][]byte
	for _, s := range status {
		if s.Code == StatusUntracked && under(s.Path, paths) {
			w.State.Added = append(w.State.Added, s.Path)
			added = append(added, s.Path)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	return added, w.SaveState()
}
