package server

import (
	"context"
	"encoding/base64"
	"encoding/binary"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/canon"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
)

// encodeLogToken serializes a log cursor: version 1 ‖ tip ‖ varint depth ‖
// u8 phase ‖ varint max depth ‖ varint seq.
func encodeLogToken(c *repo.LogCursor) string {
	b := []byte{1}
	b = append(b, c.Tip[:]...)
	b = binary.AppendVarint(b, c.Depth)
	phase := byte(0)
	if c.Mainline {
		phase = 1
	}
	b = append(b, phase)
	b = binary.AppendVarint(b, c.MaxDepth)
	b = binary.AppendVarint(b, c.BeforeSeq)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeLogToken(s string) (repo.LogCursor, error) {
	var c repo.LogCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) < 1+hash.Size || b[0] != 1 {
		return c, invalidf("malformed page token")
	}
	copy(c.Tip[:], b[1:1+hash.Size])
	r := canon.NewReader(b[1+hash.Size:])
	bad := func() (repo.LogCursor, error) { return repo.LogCursor{}, invalidf("malformed page token") }
	if c.Depth, err = r.Varint(); err != nil {
		return bad()
	}
	phase, err := r.Byte()
	if err != nil || phase > 1 {
		return bad()
	}
	c.Mainline = phase == 1
	if c.MaxDepth, err = r.Varint(); err != nil {
		return bad()
	}
	if c.BeforeSeq, err = r.Varint(); err != nil || r.Len() != 0 {
		return bad()
	}
	return c, nil
}

// Log implements RepoService: the first-parent history of revision (default
// main), optionally limited to changesets that touch path, newest first.
func (s *Server) Log(ctx context.Context, req *xylemv1.LogRequest) (*xylemv1.LogResponse, error) {
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return nil, err
	}
	var cur repo.LogCursor
	if t := req.GetPage().GetPageToken(); t != "" {
		if cur, err = decodeLogToken(t); err != nil {
			return nil, err
		}
	} else {
		tip, err := s.resolve(ctx, r, orMain(req.GetRevision()))
		if err != nil {
			return nil, err
		}
		if cur, err = r.StartLog(ctx, tip.ID); err != nil {
			return nil, err
		}
	}
	ids, next, err := r.Log(ctx, cur, req.GetPath(), pageSize(req.GetPage()))
	if err != nil {
		return nil, err
	}
	found, err := r.Changesets(ctx, ids)
	if err != nil {
		return nil, err
	}
	onMain, err := r.MainlineDepths(ctx, ids)
	if err != nil {
		return nil, err
	}
	resp := &xylemv1.LogResponse{Changesets: make([]*xylemv1.Changeset, len(ids))}
	for i, id := range ids {
		cs, ok := found[id]
		if !ok {
			return nil, notFoundf("changeset %s", id)
		}
		_, on := onMain[id]
		resp.Changesets[i] = toProtoChangeset(cs, on, false)
	}
	if next != nil {
		resp.NextPageToken = encodeLogToken(next)
	}
	return resp, nil
}
