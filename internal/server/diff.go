package server

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/diff"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/treeops"
)

// Diff limits (05 §Server implementation notes).
const (
	DefaultMaxDiffEntries = 100_000
	defaultContextLines   = 3
	diffFilesPerMessage   = 100
)

// Diff implements RepoService. With an empty from_revision it diffs
// to_revision against its first parent (the empty tree for a root). When
// from is to's first parent and there is no path filter, the change list
// comes from the changeset itself; otherwise both trees are walked together,
// skipping identical subtrees. Files stream in depth-first path order.
func (s *Server) Diff(req *xylemv1.DiffRequest, stream xylemv1.RepoService_DiffServer) error {
	ctx := stream.Context()
	if req.GetIncludeHunks() {
		if err := s.rateLimit(ctx, LimitDiffHunks); err != nil {
			return err
		}
	}
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return err
	}
	to, err := s.resolve(ctx, r, orMain(req.GetToRevision()))
	if err != nil {
		return err
	}
	var fromID hash.ID // zero: the empty tree
	if req.GetFromRevision() != "" {
		from, err := s.resolve(ctx, r, req.GetFromRevision())
		if err != nil {
			return err
		}
		fromID = from.ID
	} else if len(to.Parents) > 0 {
		fromID = to.Parents[0]
	}
	fromRoot := treeops.EmptyTree
	if !fromID.IsZero() {
		from, err := r.Changeset(ctx, fromID)
		if err != nil {
			return err
		}
		fromRoot = from.Core.RootTree
	}
	var prefixes [][]byte
	for _, p := range req.GetPaths() {
		if err := paths.ValidPath(p); err != nil {
			return invalidf("path filter: %v", err)
		}
		prefixes = append(prefixes, p)
	}

	firstParent := (len(to.Parents) > 0 && fromID == to.Parents[0]) || (len(to.Parents) == 0 && fromID.IsZero())
	var changes []treeops.Change
	if firstParent && len(prefixes) == 0 {
		changes, err = s.changesetDiff(ctx, r, fromRoot, to.Changes)
	} else {
		changes, err = treeops.Diff(ctx, r.Trees(), fromRoot, to.Core.RootTree, prefixes, s.maxDiffEntries())
	}
	if errors.Is(err, treeops.ErrTooLarge) {
		return status.Errorf(codes.ResourceExhausted, "diff scans more than %d tree entries; narrow it with paths", s.maxDiffEntries())
	}
	if err != nil {
		return err
	}

	contextLines := int(req.GetContextLines())
	if contextLines == 0 {
		contextLines = defaultContextLines
	}
	msg := &xylemv1.DiffResponse{}
	for _, c := range changes {
		fd := toFileDiff(c)
		if req.GetIncludeHunks() {
			if err := s.addHunks(ctx, fd, c, contextLines); err != nil {
				return err
			}
		}
		msg.Files = append(msg.Files, fd)
		if len(msg.Files) == diffFilesPerMessage {
			if err := stream.Send(msg); err != nil {
				return err
			}
			msg = &xylemv1.DiffResponse{}
		}
	}
	if len(msg.Files) > 0 {
		return stream.Send(msg)
	}
	return nil
}

func (s *Server) maxDiffEntries() int {
	if s.cfg.MaxDiffEntries > 0 {
		return s.cfg.MaxDiffEntries
	}
	return DefaultMaxDiffEntries
}

// changesetDiff turns a changeset's own change list (relative to its first
// parent, whose root tree is fromRoot) into tree changes.
func (s *Server) changesetDiff(ctx context.Context, r *repo.Repo, fromRoot hash.ID, cs []changeset.Change) ([]treeops.Change, error) {
	out := make([]treeops.Change, 0, len(cs))
	for _, ch := range cs {
		old, found, err := treeops.Lookup(ctx, r.Trees(), fromRoot, ch.Path)
		if err != nil {
			return nil, err
		}
		c := treeops.Change{Path: ch.Path}
		if found && old.Mode != object.ModeTree {
			c.Old = old
		}
		if ch.Op == changeset.OpPut {
			c.New = object.Entry{Mode: ch.Mode, ID: ch.Blob, Size: ch.Size}
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b treeops.Change) int { return treeops.ComparePathsDFS(a.Path, b.Path) })
	return out, nil
}

func toFileDiff(c treeops.Change) *xylemv1.FileDiff {
	fd := &xylemv1.FileDiff{Path: c.Path, OldMode: toProtoMode(c.Old.Mode), NewMode: toProtoMode(c.New.Mode)}
	if c.Old.Mode != 0 {
		fd.OldBlob = c.Old.ID[:]
	}
	if c.New.Mode != 0 {
		fd.NewBlob = c.New.ID[:]
	}
	switch {
	case c.Old.Mode == 0:
		fd.Status = xylemv1.FileDiff_STATUS_ADDED
	case c.New.Mode == 0:
		fd.Status = xylemv1.FileDiff_STATUS_DELETED
	case c.Old.ID == c.New.ID:
		fd.Status = xylemv1.FileDiff_STATUS_MODE_CHANGED
	default:
		fd.Status = xylemv1.FileDiff_STATUS_MODIFIED
	}
	return fd
}

// addHunks fills hunks, or marks the file binary (NUL in the first 8 KiB of
// either side).
func (s *Server) addHunks(ctx context.Context, fd *xylemv1.FileDiff, c treeops.Change, contextLines int) error {
	if c.Old.ID == c.New.ID {
		return nil // mode change only
	}
	var old, new []byte
	var err error
	if c.Old.Mode != 0 {
		if old, err = s.readBlob(ctx, c.Old.ID); err != nil {
			return err
		}
	}
	if c.New.Mode != 0 {
		if new, err = s.readBlob(ctx, c.New.ID); err != nil {
			return err
		}
	}
	if diff.IsBinary(old) || diff.IsBinary(new) {
		fd.Binary = true
		return nil
	}
	for _, h := range diff.Hunks(old, new, contextLines) {
		fd.Hunks = append(fd.Hunks, &xylemv1.DiffHunk{
			OldStart: clampUint32(h.OldStart), OldLines: clampUint32(h.OldLines),
			NewStart: clampUint32(h.NewStart), NewLines: clampUint32(h.NewLines),
			Body: bytes.Clone(h.Body),
		})
	}
	return nil
}
