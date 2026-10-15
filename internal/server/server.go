// Package server holds thin gRPC adapters over package repo, plus
// interceptors and the health and metrics HTTP endpoints (05-api.md).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/auth"
	"github.com/delefrati/xylem/internal/cache"
	"github.com/delefrati/xylem/internal/derived"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/store"
)

// DefaultRepo is the repository used when a request leaves repo empty.
const DefaultRepo = "default"

// MaxMessageSize is the gRPC send and receive limit on every tier (05
// §Conventions): larger than the 64 MiB maximum blob.
const MaxMessageSize = 80 << 20

// Config configures a Server.
type Config struct {
	Meta        meta.Store
	Objects     store.ObjectStore
	RepoOptions repo.Options
	Logger      *slog.Logger
	Registry    *prometheus.Registry // metrics; a new one if nil
	// Auth enforces bearer tokens and per-RPC roles (M9.1); nil allows
	// every call.
	Auth *auth.Tokens
	// ObjectCacheBytes bounds the in-memory cache of stored object frames.
	ObjectCacheBytes int64
	// Ready reports whether dependencies are reachable (for /readyz).
	Ready func(ctx context.Context) error
	// MaxDiffEntries bounds the tree entries one Diff may scan (default
	// DefaultMaxDiffEntries); beyond it Diff fails with ResourceExhausted.
	MaxDiffEntries int
	// MaxAncestryNodes bounds the changesets IsAncestor and MergeBase may
	// visit (default DefaultMaxAncestryNodes).
	MaxAncestryNodes int64
}

// Server implements RepoService, ObjectService and WriteService.
type Server struct {
	xylemv1.UnimplementedRepoServiceServer
	xylemv1.UnimplementedObjectServiceServer
	xylemv1.UnimplementedWriteServiceServer

	cfg     Config
	log     *slog.Logger
	metrics *rpcMetrics
	objects *cache.Memory

	mu    sync.Mutex
	repos map[string]*repo.Repo
}

// New returns a Server.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Registry == nil {
		cfg.Registry = prometheus.NewRegistry()
	}
	metrics := newRPCMetrics(cfg.Registry)
	next := cfg.RepoOptions.OnMerge
	cfg.RepoOptions.OnMerge = func(clean bool) {
		if clean {
			metrics.mergeClean.Inc()
		} else {
			metrics.mergeConflict.Inc()
		}
		if next != nil {
			next(clean)
		}
	}
	return &Server{
		cfg: cfg, log: cfg.Logger, metrics: metrics,
		objects: cache.NewMemory(cfg.ObjectCacheBytes), repos: map[string]*repo.Repo{},
	}
}

// Registry returns the metrics registry.
func (s *Server) Registry() *prometheus.Registry { return s.cfg.Registry }

// NewGRPCServer returns a gRPC server with all services, interceptors and
// reflection registered.
func (s *Server) NewGRPCServer(opts ...grpc.ServerOption) *grpc.Server {
	opts = append([]grpc.ServerOption{
		grpc.MaxRecvMsgSize(MaxMessageSize),
		grpc.MaxSendMsgSize(MaxMessageSize),
		grpc.ChainUnaryInterceptor(s.unaryInterceptor),
		grpc.ChainStreamInterceptor(s.streamInterceptor),
	}, opts...)
	g := grpc.NewServer(opts...)
	xylemv1.RegisterRepoServiceServer(g, s)
	xylemv1.RegisterObjectServiceServer(g, s)
	xylemv1.RegisterWriteServiceServer(g, s)
	reflection.Register(g)
	return g
}

// repo returns the repository called name (DefaultRepo if empty), opening
// and caching it on first use.
func (s *Server) repo(ctx context.Context, name string) (*repo.Repo, error) {
	if name == "" {
		name = DefaultRepo
	}
	s.mu.Lock()
	r, ok := s.repos[name]
	s.mu.Unlock()
	if ok {
		return r, nil
	}
	r, err := repo.Open(ctx, s.cfg.Meta, s.cfg.Objects, name, s.cfg.RepoOptions)
	if err != nil {
		return nil, toStatus(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.repos[name]; ok {
		return existing, nil
	}
	s.repos[name] = r
	return r, nil
}

// toStatus maps domain errors to gRPC status codes (05 §Error model).
func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	var missing *repo.MissingBlobsError
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.As(err, &missing):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, repo.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, repo.ErrNotFound), errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, repo.ErrConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, repo.ErrNotFastForward):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, repo.ErrBinary):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, derived.ErrTooExpensive):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, derived.ErrMissing):
		return status.Error(codes.NotFound, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func invalidf(format string, args ...any) error {
	return status.Error(codes.InvalidArgument, fmt.Sprintf(format, args...))
}

func notFoundf(format string, args ...any) error {
	return status.Error(codes.NotFound, fmt.Sprintf(format, args...))
}
