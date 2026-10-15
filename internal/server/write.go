package server

import (
	"context"
	"fmt"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
)

// CreateChangeset implements WriteService (07 §CreateChangeset). The
// committer is the author at the server's clock until M9 sets it from the
// authenticated identity. Validation failures are InvalidArgument, missing
// blobs FailedPrecondition (up to 20 ids listed) and unknown parents NotFound.
func (s *Server) CreateChangeset(ctx context.Context, req *xylemv1.CreateChangesetRequest) (*xylemv1.CreateChangesetResponse, error) {
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return nil, err
	}
	cr := repo.CreateRequest{Message: req.GetMessage(), Extras: req.GetExtras()}
	if cr.Author, err = fromProtoSignature(req.GetAuthor()); err != nil {
		return nil, err
	}
	cr.Parents = make([]hash.ID, len(req.GetParents()))
	for i, p := range req.GetParents() {
		if cr.Parents[i], err = idFrom(p, fmt.Sprintf("parents[%d]", i)); err != nil {
			return nil, err
		}
	}
	cr.Changes = make([]changeset.Change, len(req.GetChanges()))
	for i, fc := range req.GetChanges() {
		if cr.Changes[i], err = fromProtoChange(fc); err != nil {
			return nil, err
		}
	}
	// TODO(M9): committer = the authenticated identity.
	cs, existed, err := r.CreateChangeset(ctx, cr, repo.CreateOptions{})
	if err != nil {
		return nil, err
	}
	on := false
	if existed {
		onMain, err := r.MainlineDepths(ctx, []hash.ID{cs.ID})
		if err != nil {
			return nil, err
		}
		_, on = onMain[cs.ID]
	}
	return &xylemv1.CreateChangesetResponse{Changeset: toProtoChangeset(cs, on, false), AlreadyExisted: existed}, nil
}
