package server

import (
	"context"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
)

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
		resp.Refs[i] = &xylemv1.Ref{Name: ref.Name, Id: ref.ID[:], Version: uint64(max(ref.Version, 0))}
	}
	return resp, nil
}
