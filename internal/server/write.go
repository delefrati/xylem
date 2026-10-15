package server

import (
	"context"
	"fmt"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/auth"
	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
)

// CreateChangeset implements WriteService (07 §CreateChangeset). The
// committer is the authenticated identity (the author when auth is off) at
// the server's clock. Validation failures are InvalidArgument, missing
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
	cs, existed, err := r.CreateChangeset(ctx, cr, repo.CreateOptions{CommitterName: committerOf(ctx)})
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

// committerOf is the authenticated identity as a committer, or nil (the
// author) when auth is off.
func committerOf(ctx context.Context) []byte {
	if id, ok := auth.FromContext(ctx); ok {
		return []byte(id.Name)
	}
	return nil
}

// MaxLandAttempts caps a request's max_attempts.
const MaxLandAttempts = 1000

func stackIDs(raw [][]byte) ([]hash.ID, error) {
	if len(raw) > repo.MaxStack {
		return nil, invalidf("stack of %d changesets, max %d", len(raw), repo.MaxStack)
	}
	ids := make([]hash.ID, len(raw))
	for i, b := range raw {
		var err error
		if ids[i], err = idFrom(b, fmt.Sprintf("stack[%d]", i)); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func idsBytes(ids []hash.ID) [][]byte {
	out := make([][]byte, len(ids))
	for i := range ids {
		out[i] = ids[i][:]
	}
	return out
}

var conflictKinds = map[repo.ConflictKind]xylemv1.Conflict_Kind{
	repo.ConflictContent: xylemv1.Conflict_KIND_CONTENT, repo.ConflictDeleteModify: xylemv1.Conflict_KIND_DELETE_MODIFY,
	repo.ConflictAddAdd: xylemv1.Conflict_KIND_ADD_ADD, repo.ConflictFileDirectory: xylemv1.Conflict_KIND_FILE_DIRECTORY,
	repo.ConflictMode: xylemv1.Conflict_KIND_MODE,
}

func toProtoConflicts(cs []repo.Conflict) []*xylemv1.Conflict {
	out := make([]*xylemv1.Conflict, len(cs))
	for i, c := range cs {
		pc := &xylemv1.Conflict{Path: c.Path, Kind: conflictKinds[c.Kind], RegionCount: clampUint32(c.Regions)}
		if !c.Base.IsZero() {
			pc.BaseBlob = c.Base[:]
		}
		if !c.Ours.IsZero() {
			pc.OursBlob = c.Ours[:]
		}
		if !c.Theirs.IsZero() {
			pc.TheirsBlob = c.Theirs[:]
		}
		out[i] = pc
	}
	return out
}

// Rebase implements WriteService (07 §Rebase). Conflicts are returned in the
// response, not as an error.
func (s *Server) Rebase(ctx context.Context, req *xylemv1.RebaseRequest) (*xylemv1.RebaseResponse, error) {
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return nil, err
	}
	stack, err := stackIDs(req.GetStack())
	if err != nil {
		return nil, err
	}
	onto, err := s.resolve(ctx, r, req.GetOnto())
	if err != nil {
		return nil, err
	}
	res, err := r.RebaseStack(ctx, stack, onto.ID, repo.RebaseOptions{Committer: committerOf(ctx)})
	if err != nil {
		return nil, err
	}
	resp := &xylemv1.RebaseResponse{Rebased: idsBytes(res.Rebased), Skipped: idsBytes(res.Skipped), Conflicts: toProtoConflicts(res.Conflicts)}
	if len(res.Conflicts) > 0 {
		resp.ConflictingChangeset = res.Conflicting[:]
	}
	return resp, nil
}

// Land implements WriteService (07 §Land). Conflicts are returned in the
// response; running out of attempts is Aborted.
func (s *Server) Land(ctx context.Context, req *xylemv1.LandRequest) (*xylemv1.LandResponse, error) {
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return nil, err
	}
	stack, err := stackIDs(req.GetStack())
	if err != nil {
		return nil, err
	}
	if req.GetMaxAttempts() > MaxLandAttempts {
		return nil, invalidf("max_attempts %d, max %d", req.GetMaxAttempts(), MaxLandAttempts)
	}
	res, err := r.Land(ctx, stack, repo.LandOptions{
		Target: req.GetTargetRef(), MaxAttempts: int(req.GetMaxAttempts()),
		Committer: committerOf(ctx), Actor: actorOf(ctx),
	})
	if err != nil {
		return nil, err
	}
	s.metrics.landAttempts.Observe(float64(res.Attempts))
	resp := &xylemv1.LandResponse{
		Landed: idsBytes(res.Landed), NewTip: res.NewTip[:], Skipped: idsBytes(res.Skipped),
		Conflicts: toProtoConflicts(res.Conflicts), Attempts: clampUint32(res.Attempts),
	}
	if len(res.Conflicts) > 0 {
		s.metrics.landConflicts.Inc()
		resp.ConflictingChangeset, resp.NewTip = res.Conflicting[:], nil
	}
	return resp, nil
}
