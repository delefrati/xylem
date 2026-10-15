package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/hash"
)

// TierHeader names the tier that answered a call, in response headers and
// trailers, so a tier can check what its upstream is (09: strict tiering).
const TierHeader = "xylem-tier"

// Tier kinds.
const (
	KindServer = "server"
	KindCache  = "cache"
	KindDaemon = "daemon"
)

// Limits of the ObjectService wire protocol (05 §Conventions).
const (
	MaxBatchIDs    = 1024
	MaxMessageSize = 80 << 20
)

// UpstreamKind asks upstream which tier it is, with a call that needs no
// data (HasObjects of no ids).
func UpstreamKind(ctx context.Context, upstream xylemv1.ObjectServiceClient) (string, error) {
	var header, trailer metadata.MD
	_, err := upstream.HasObjects(ctx, &xylemv1.HasObjectsRequest{}, grpc.Header(&header), grpc.Trailer(&trailer))
	for _, md := range []metadata.MD{header, trailer} {
		if v := md.Get(TierHeader); len(v) > 0 {
			return v[0], nil
		}
	}
	if err != nil {
		return "", err
	}
	return "", errors.New("upstream does not say which tier it is (not a xylemd server, cache or daemon?)")
}

// CheckUpstream fails unless upstream is of kind want.
func CheckUpstream(ctx context.Context, upstream xylemv1.ObjectServiceClient, want string) error {
	got, err := UpstreamKind(ctx, upstream)
	if err != nil {
		return fmt.Errorf("checking upstream: %w", err)
	}
	if got != want {
		return fmt.Errorf("upstream is a %s, want a %s (tiering is daemon → cache → server)", got, want)
	}
	return nil
}

// Service serves ObjectService from a Tier.
type Service struct {
	xylemv1.UnimplementedObjectServiceServer
	tier *Tier
	kind string
	log  *slog.Logger
}

// NewService returns the ObjectService of a tier of the given kind.
func NewService(tier *Tier, kind string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{tier: tier, kind: kind, log: log}
}

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

// GetObjects streams the objects packed up to ~1 MiB per message (a larger
// one goes alone), in request order, with missing ids in the last message.
func (s *Service) GetObjects(req *xylemv1.GetObjectsRequest, stream xylemv1.ObjectService_GetObjectsServer) error {
	ids, err := parseIDs(req.GetIds())
	if err != nil {
		return err
	}
	got, err := s.tier.Get(stream.Context(), req.GetRepo(), ids)
	if err != nil {
		return err
	}
	msg, size := &xylemv1.GetObjectsResponse{}, 0
	var missing [][]byte
	seen := make(map[hash.ID]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		data, ok := got[id]
		if !ok {
			missing = append(missing, id[:])
			continue
		}
		if size > 0 && size+len(data) > packTarget {
			if err := stream.Send(msg); err != nil {
				return err
			}
			msg, size = &xylemv1.GetObjectsResponse{}, 0
		}
		msg.Objects = append(msg.Objects, &xylemv1.StoredObject{Id: id[:], Data: data})
		size += len(data)
	}
	msg.Missing = missing
	if len(msg.Objects) == 0 && len(msg.Missing) == 0 {
		return nil
	}
	return stream.Send(msg)
}

// HasObjects answers from the cache, asking upstream for the rest.
func (s *Service) HasObjects(ctx context.Context, req *xylemv1.HasObjectsRequest) (*xylemv1.HasObjectsResponse, error) {
	ids, err := parseIDs(req.GetIds())
	if err != nil {
		return nil, err
	}
	present, err := s.tier.Has(ctx, req.GetRepo(), ids)
	if err != nil {
		return nil, err
	}
	return &xylemv1.HasObjectsResponse{Present: present}, nil
}

// PutObjects collects the stream, then writes it through upstream.
func (s *Service) PutObjects(stream xylemv1.ObjectService_PutObjectsServer) error {
	var objs []*xylemv1.StoredObject
	repo := ""
	for first := true; ; first = false {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if first {
			repo = msg.GetRepo()
		} else if msg.GetRepo() != "" && msg.GetRepo() != repo {
			return status.Errorf(codes.InvalidArgument, "repo %q differs from the stream's %q", msg.GetRepo(), repo)
		}
		if len(msg.GetObjects()) > MaxBatchIDs {
			return status.Errorf(codes.InvalidArgument, "%d objects in one message, max %d", len(msg.GetObjects()), MaxBatchIDs)
		}
		objs = append(objs, msg.GetObjects()...)
	}
	resp, err := s.tier.Put(stream.Context(), repo, objs)
	if err != nil {
		return err
	}
	return stream.SendAndClose(resp)
}

// toStatus maps tier errors to gRPC codes; upstream statuses pass through.
func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, ErrOverloaded):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrCorrupt):
		return status.Error(codes.Unavailable, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

// Interceptors stamp the tier kind on every response, recover panics, map
// errors and log each call.
func (s *Service) Interceptors() []grpc.ServerOption {
	stamp := func(ctx context.Context) {
		md := metadata.Pairs(TierHeader, s.kind)
		_ = grpc.SetHeader(ctx, md)
		_ = grpc.SetTrailer(ctx, md)
	}
	done := func(ctx context.Context, method string, start time.Time, err error) {
		code := status.Code(err)
		attrs := []any{"method", method, "code", code.String(), "duration_ms", float64(time.Since(start).Microseconds()) / 1000}
		if err != nil && code != codes.NotFound && code != codes.InvalidArgument && code != codes.Canceled {
			s.log.WarnContext(ctx, "rpc", append(attrs, "error", err.Error())...)
		} else {
			s.log.DebugContext(ctx, "rpc", attrs...)
		}
	}
	recovered := func(ctx context.Context, method string, p any) error {
		s.log.ErrorContext(ctx, "panic in rpc", "method", method, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
		return status.Error(codes.Internal, "internal error")
	}
	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		start := time.Now()
		stamp(ctx)
		defer func() {
			if p := recover(); p != nil {
				resp, err = nil, recovered(ctx, info.FullMethod, p)
			}
			done(ctx, info.FullMethod, start, err)
		}()
		resp, err = handler(ctx, req)
		return resp, toStatus(err)
	}
	streaming := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		start := time.Now()
		stamp(ss.Context())
		defer func() {
			if p := recover(); p != nil {
				err = recovered(ss.Context(), info.FullMethod, p)
			}
			done(ss.Context(), info.FullMethod, start, err)
		}()
		return toStatus(handler(srv, ss))
	}
	return []grpc.ServerOption{
		grpc.MaxRecvMsgSize(MaxMessageSize), grpc.MaxSendMsgSize(MaxMessageSize),
		grpc.ChainUnaryInterceptor(unary), grpc.ChainStreamInterceptor(streaming),
	}
}

// NewGRPCServer returns a gRPC server with the tier's ObjectService; extra
// options add, for example, the daemon's Proxy.
func (s *Service) NewGRPCServer(extra ...grpc.ServerOption) *grpc.Server {
	g := grpc.NewServer(append(s.Interceptors(), extra...)...)
	xylemv1.RegisterObjectServiceServer(g, s)
	return g
}
