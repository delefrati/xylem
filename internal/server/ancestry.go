package server

import (
	"context"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/derived"
	"github.com/delefrati/xylem/internal/revision"
)

// DefaultMaxAncestryNodes bounds the changesets one IsAncestor or MergeBase
// call may visit before failing with ResourceExhausted.
const DefaultMaxAncestryNodes = 1_000_000

func (s *Server) maxAncestryNodes() int64 {
	if s.cfg.MaxAncestryNodes > 0 {
		return s.cfg.MaxAncestryNodes
	}
	return DefaultMaxAncestryNodes
}

// IsAncestor implements RepoService.
func (s *Server) IsAncestor(ctx context.Context, req *xylemv1.IsAncestorRequest) (*xylemv1.IsAncestorResponse, error) {
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return nil, err
	}
	a, err := revision.Resolve(ctx, r, req.GetAncestor())
	if err != nil {
		return nil, err
	}
	b, err := revision.Resolve(ctx, r, req.GetDescendant())
	if err != nil {
		return nil, err
	}
	ok, err := derived.IsAncestor(ctx, derived.Limited(r.Source(), s.maxAncestryNodes()), a, b)
	if err != nil {
		return nil, err
	}
	return &xylemv1.IsAncestorResponse{Result: ok}, nil
}

// MergeBase implements RepoService: the best common ancestors, as
// `git merge-base --all`, sorted by generation descending then id.
func (s *Server) MergeBase(ctx context.Context, req *xylemv1.MergeBaseRequest) (*xylemv1.MergeBaseResponse, error) {
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return nil, err
	}
	a, err := revision.Resolve(ctx, r, req.GetA())
	if err != nil {
		return nil, err
	}
	b, err := revision.Resolve(ctx, r, req.GetB())
	if err != nil {
		return nil, err
	}
	bases, err := derived.MergeBases(ctx, derived.Limited(r.Source(), s.maxAncestryNodes()), a, b)
	if err != nil {
		return nil, err
	}
	resp := &xylemv1.MergeBaseResponse{Ids: make([][]byte, len(bases))}
	for i := range bases {
		resp.Ids[i] = bases[i][:]
	}
	return resp, nil
}
