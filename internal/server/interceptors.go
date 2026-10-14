package server

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// RequestIDHeader carries the request id in and out (gRPC metadata).
const RequestIDHeader = "x-request-id"

type requestIDKey struct{}

// RequestID returns the id of the request being served, if any.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type rpcMetrics struct {
	duration *prometheus.HistogramVec
}

func newRPCMetrics(reg prometheus.Registerer) *rpcMetrics {
	m := &rpcMetrics{duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "xylem_rpc_duration_seconds",
		Help:    "RPC latency by method and status code.",
		Buckets: prometheus.ExponentialBuckets(0.0005, 2, 16), // 0.5 ms .. ~16 s
	}, []string{"method", "code"})}
	reg.MustRegister(m.duration)
	return m
}

// repoNamer is implemented by every request message that carries a repo.
type repoNamer interface{ GetRepo() string }

// begin sets up the request id and runs the auth hook.
func (s *Server) begin(ctx context.Context, method string) (context.Context, error) {
	id := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(RequestIDHeader); len(v) > 0 && len(v[0]) <= 128 {
			id = v[0]
		}
	}
	if id == "" {
		var b [8]byte
		_, _ = cryptorand.Read(b[:])
		id = hex.EncodeToString(b[:])
	}
	ctx = context.WithValue(ctx, requestIDKey{}, id)
	_ = grpc.SetHeader(ctx, metadata.Pairs(RequestIDHeader, id))
	if s.cfg.Auth != nil {
		if err := s.cfg.Auth(ctx, method); err != nil {
			return ctx, status.Error(codes.Unauthenticated, err.Error())
		}
	}
	return ctx, nil
}

// end records the access log line and metrics.
func (s *Server) end(ctx context.Context, method, repoName string, start time.Time, err error) {
	code := status.Code(err)
	elapsed := time.Since(start)
	s.metrics.duration.WithLabelValues(method, code.String()).Observe(elapsed.Seconds())
	attrs := []any{
		"method", method, "repo", repoName, "code", code.String(),
		"duration_ms", float64(elapsed.Microseconds()) / 1000, "request_id", RequestID(ctx),
	}
	if err != nil && code != codes.NotFound && code != codes.InvalidArgument && code != codes.Canceled {
		s.log.WarnContext(ctx, "rpc", append(attrs, "error", err.Error())...)
	} else {
		s.log.InfoContext(ctx, "rpc", attrs...)
	}
}

// recovered turns a panic into an Internal error and logs the stack.
func (s *Server) recovered(ctx context.Context, method string, p any) error {
	s.log.ErrorContext(ctx, "panic in rpc", "method", method, "panic", fmt.Sprint(p), "stack", string(debug.Stack()), "request_id", RequestID(ctx))
	return status.Error(codes.Internal, "internal error")
}

func (s *Server) unaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	start := time.Now()
	repoName := ""
	if r, ok := req.(repoNamer); ok {
		repoName = r.GetRepo()
	}
	ctx, err = s.begin(ctx, info.FullMethod)
	defer func() {
		if p := recover(); p != nil {
			resp, err = nil, s.recovered(ctx, info.FullMethod, p)
		}
		s.end(ctx, info.FullMethod, repoName, start, err)
	}()
	if err != nil {
		return nil, err
	}
	resp, err = handler(ctx, req)
	return resp, toStatus(err)
}

// serverStream overrides the context of a stream.
type serverStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *serverStream) Context() context.Context { return s.ctx }

func (s *Server) streamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	start := time.Now()
	ctx, err := s.begin(ss.Context(), info.FullMethod)
	defer func() {
		if p := recover(); p != nil {
			err = s.recovered(ctx, info.FullMethod, p)
		}
		s.end(ctx, info.FullMethod, "", start, err)
	}()
	if err != nil {
		return err
	}
	return toStatus(handler(srv, &serverStream{ServerStream: ss, ctx: ctx}))
}
