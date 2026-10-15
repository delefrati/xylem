package server

import (
	"context"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
	"github.com/delefrati/xylem/internal/treeops"
)

// Blame implements RepoService: each line of path at revision (default main)
// with the changeset that introduced it, following first-parent history.
// Binary files are FailedPrecondition.
func (s *Server) Blame(ctx context.Context, req *xylemv1.BlameRequest) (*xylemv1.BlameResponse, error) {
	if err := s.rateLimit(ctx, LimitBlame); err != nil {
		return nil, err
	}
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return nil, err
	}
	if err := paths.ValidPath(req.GetPath()); err != nil {
		return nil, invalidf("%v", err)
	}
	cs, err := s.resolve(ctx, r, orMain(req.GetRevision()))
	if err != nil {
		return nil, err
	}
	e, found, err := treeops.Lookup(ctx, r.Trees(), cs.Core.RootTree, req.GetPath())
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, notFoundf("path %q at %s", req.GetPath(), cs.ID.Short(12))
	}
	if e.Mode == object.ModeTree {
		return nil, invalidf("%q is a directory", req.GetPath())
	}
	res, err := r.Blame(ctx, cs.ID, req.GetPath())
	if err != nil {
		return nil, err
	}
	resp := &xylemv1.BlameResponse{Lines: make([]*xylemv1.BlameLine, len(res.Lines))}
	for i, l := range res.Lines {
		resp.Lines[i] = &xylemv1.BlameLine{
			Origin:     l.Origin[:],
			OriginLine: clampUint32(l.Line),
			Content:    trimNewline(res.Content[i]),
		}
	}
	origins, err := r.Changesets(ctx, res.Origins)
	if err != nil {
		return nil, err
	}
	onMain, err := r.MainlineDepths(ctx, res.Origins)
	if err != nil {
		return nil, err
	}
	for _, id := range res.Origins {
		if o, ok := origins[id]; ok {
			_, on := onMain[id]
			resp.Origins = append(resp.Origins, toProtoChangeset(o, on, false))
		}
	}
	return resp, nil
}

// trimNewline drops a line's terminator (05: content is the line without "\n").
func trimNewline(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\n' {
		return line[:n-1]
	}
	return line
}
