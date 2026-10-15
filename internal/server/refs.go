package server

import (
	"context"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/auth"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/revision"
)

// MaxGetChangesets is the batch limit of GetChangesets (05).
const MaxGetChangesets = 1000

// ResolveRevision implements RepoService.
func (s *Server) ResolveRevision(ctx context.Context, req *xylemv1.ResolveRevisionRequest) (*xylemv1.ResolveRevisionResponse, error) {
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return nil, err
	}
	cs, err := s.resolve(ctx, r, req.GetRevision())
	if err != nil {
		return nil, err
	}
	return &xylemv1.ResolveRevisionResponse{Id: cs.ID[:], RootTree: cs.Core.RootTree[:]}, nil
}

// resolve resolves a revision to a stored changeset.
func (s *Server) resolve(ctx context.Context, r *repo.Repo, rev string) (*repo.Changeset, error) {
	if rev == "" {
		return nil, invalidf("revision is required")
	}
	id, err := revision.Resolve(ctx, r, rev)
	if err != nil {
		return nil, err
	}
	return r.Changeset(ctx, id)
}

// GetChangesets implements RepoService. Results are in request order; an
// unknown id is NotFound.
func (s *Server) GetChangesets(ctx context.Context, req *xylemv1.GetChangesetsRequest) (*xylemv1.GetChangesetsResponse, error) {
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return nil, err
	}
	if len(req.GetIds()) > MaxGetChangesets {
		return nil, invalidf("%d ids in one call, max %d", len(req.GetIds()), MaxGetChangesets)
	}
	ids := make([]hash.ID, len(req.GetIds()))
	for i, b := range req.GetIds() {
		if ids[i], err = idFrom(b, "ids"); err != nil {
			return nil, err
		}
	}
	found, err := r.Changesets(ctx, ids)
	if err != nil {
		return nil, err
	}
	onMain, err := r.MainlineDepths(ctx, ids)
	if err != nil {
		return nil, err
	}
	resp := &xylemv1.GetChangesetsResponse{Changesets: make([]*xylemv1.Changeset, len(ids))}
	for i, id := range ids {
		cs, ok := found[id]
		if !ok {
			return nil, notFoundf("changeset %s", id)
		}
		_, on := onMain[id]
		resp.Changesets[i] = toProtoChangeset(cs, on, req.GetIncludeChanges())
	}
	return resp, nil
}

// ListRefs implements RepoService.
func (s *Server) ListRefs(ctx context.Context, req *xylemv1.ListRefsRequest) (*xylemv1.ListRefsResponse, error) {
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return nil, err
	}
	refs, err := r.ListRefs(ctx, req.GetPrefix())
	if err != nil {
		return nil, err
	}
	resp := &xylemv1.ListRefsResponse{Refs: make([]*xylemv1.Ref, len(refs))}
	for i, ref := range refs {
		resp.Refs[i] = toProtoRef(ref)
	}
	return resp, nil
}

// UpdateRef implements RepoService with admin semantics (07 §UpdateRef): an
// empty expected_id creates the ref; a stale one is Aborted; a
// non-fast-forward without force is FailedPrecondition.
func (s *Server) UpdateRef(ctx context.Context, req *xylemv1.UpdateRefRequest) (*xylemv1.UpdateRefResponse, error) {
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return nil, err
	}
	var expected hash.ID
	if len(req.GetExpectedId()) > 0 {
		if expected, err = idFrom(req.GetExpectedId(), "expected_id"); err != nil {
			return nil, err
		}
	}
	target, err := idFrom(req.GetNewId(), "new_id")
	if err != nil {
		return nil, err
	}
	ref, err := r.UpdateRef(ctx, req.GetName(), expected, target, req.GetForce(), actorOf(ctx), "admin")
	if err != nil {
		return nil, err
	}
	return &xylemv1.UpdateRefResponse{Ref: toProtoRef(ref)}, nil
}

// actorOf names the caller for the ref log: the authenticated identity, or
// the request id when auth is off.
func actorOf(ctx context.Context) string {
	if id, ok := auth.FromContext(ctx); ok {
		return id.Name
	}
	return "rpc:" + RequestID(ctx)
}
