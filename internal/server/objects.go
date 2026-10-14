package server

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/store"
)

// Object batch and message limits (05 §Conventions).
const (
	MaxBatchIDs     = 1024
	packTargetBytes = 1 << 20 // objects are packed up to ~1 MiB per message
	readParallelism = 16
)

// parseIDs validates a batch of ids.
func parseIDs(raw [][]byte) ([]hash.ID, error) {
	if len(raw) > MaxBatchIDs {
		return nil, status.Errorf(codes.InvalidArgument, "%d ids in one call, max %d", len(raw), MaxBatchIDs)
	}
	ids := make([]hash.ID, len(raw))
	for i, b := range raw {
		if len(b) != hash.Size {
			return nil, status.Errorf(codes.InvalidArgument, "id %d has %d bytes, want %d", i, len(b), hash.Size)
		}
		copy(ids[i][:], b)
	}
	return ids, nil
}

type fetched struct {
	id   hash.ID
	data []byte // nil if missing
}

// GetObjects implements ObjectService: objects are read in parallel (cache
// first), streamed in completion order packed up to ~1 MiB per message, and
// ids not found are listed in the final message.
func (s *Server) GetObjects(req *xylemv1.GetObjectsRequest, stream xylemv1.ObjectService_GetObjectsServer) error {
	ctx := stream.Context()
	if _, err := s.repo(ctx, req.GetRepo()); err != nil {
		return err
	}
	ids, err := parseIDs(req.GetIds())
	if err != nil {
		return err
	}

	results := make(chan fetched, readParallelism)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		defer close(results)
		rg, rctx := errgroup.WithContext(gctx)
		rg.SetLimit(readParallelism)
		for _, id := range ids {
			rg.Go(func() error {
				data, err := s.readObject(rctx, id)
				if err != nil {
					return err
				}
				select {
				case results <- fetched{id: id, data: data}:
					return nil
				case <-rctx.Done():
					return rctx.Err()
				}
			})
		}
		return rg.Wait()
	})
	g.Go(func() error {
		var (
			msg     = &xylemv1.GetObjectsResponse{}
			size    int
			missing [][]byte
		)
		flush := func() error {
			if len(msg.Objects) == 0 {
				return nil
			}
			err := stream.Send(msg)
			msg, size = &xylemv1.GetObjectsResponse{}, 0
			return err
		}
		for f := range results {
			if f.data == nil {
				missing = append(missing, f.id[:])
				continue
			}
			if size > 0 && size+len(f.data) > packTargetBytes {
				if err := flush(); err != nil {
					return err
				}
			}
			msg.Objects = append(msg.Objects, &xylemv1.StoredObject{Id: f.id[:], Data: f.data})
			size += len(f.data)
		}
		msg.Missing = missing
		if len(msg.Objects) == 0 && len(missing) == 0 && size == 0 {
			return nil // nothing left to say
		}
		return stream.Send(msg)
	})
	return g.Wait()
}

// readObject returns an object's stored frame from the cache or the store,
// or nil if the store does not have it.
func (s *Server) readObject(ctx context.Context, id hash.ID) ([]byte, error) {
	if data, ok := s.objects.get(id); ok {
		return data, nil
	}
	data, err := s.cfg.Objects.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading object %s: %w", id, err)
	}
	s.objects.add(id, data)
	return data, nil
}

// HasObjects implements ObjectService.
func (s *Server) HasObjects(ctx context.Context, req *xylemv1.HasObjectsRequest) (*xylemv1.HasObjectsResponse, error) {
	if _, err := s.repo(ctx, req.GetRepo()); err != nil {
		return nil, err
	}
	ids, err := parseIDs(req.GetIds())
	if err != nil {
		return nil, err
	}
	present := make([]bool, len(ids))
	var ask []hash.ID
	var askAt []int
	for i, id := range ids {
		if s.objects.has(id) {
			present[i] = true
		} else {
			ask = append(ask, id)
			askAt = append(askAt, i)
		}
	}
	if len(ask) > 0 {
		got, err := s.cfg.Objects.Has(ctx, ask)
		if err != nil {
			return nil, err
		}
		for j, ok := range got {
			present[askAt[j]] = ok
		}
	}
	return &xylemv1.HasObjectsResponse{Present: present}, nil
}

// PutObjects is not available until M6 (the write path).
func (s *Server) PutObjects(xylemv1.ObjectService_PutObjectsServer) error {
	return status.Error(codes.Unimplemented, "PutObjects arrives with the write path (M6)") // TODO(M6): verify and store uploads
}
