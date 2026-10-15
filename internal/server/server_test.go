package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/delefrati/xylem/internal/auth"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/store"
)

func TestToStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want codes.Code
	}{
		{nil, codes.OK},
		{fmt.Errorf("x: %w", repo.ErrInvalid), codes.InvalidArgument},
		{fmt.Errorf("x: %w", repo.ErrNotFound), codes.NotFound},
		{fmt.Errorf("x: %w", store.ErrNotFound), codes.NotFound},
		{fmt.Errorf("x: %w", repo.ErrConflict), codes.Aborted},
		{fmt.Errorf("x: %w", repo.ErrNotFastForward), codes.FailedPrecondition},
		{&repo.MissingBlobsError{Total: 1}, codes.FailedPrecondition},
		{context.Canceled, codes.Canceled},
		{context.DeadlineExceeded, codes.DeadlineExceeded},
		{status.Error(codes.ResourceExhausted, "x"), codes.ResourceExhausted},
		{errors.New("boom"), codes.Internal},
	}
	for _, tt := range tests {
		if got := status.Code(toStatus(tt.err)); got != tt.want {
			t.Errorf("toStatus(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// fakeStream is a minimal grpc.ServerStream for interceptor tests.
type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeStream) Context() context.Context { return f.ctx }

func TestInterceptors(t *testing.T) {
	t.Parallel()
	s := New(Config{})
	info := &grpc.UnaryServerInfo{FullMethod: "/xylem.v1.RepoService/Test"}

	// Panics become Internal.
	_, err := s.unaryInterceptor(context.Background(), nil, info, func(context.Context, any) (any, error) { panic("kaboom") })
	if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "kaboom") {
		t.Errorf("unary panic: %v (want Internal without leaking the panic)", err)
	}
	err = s.streamInterceptor(nil, &fakeStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/x/Stream"},
		func(any, grpc.ServerStream) error { panic("stream kaboom") })
	if status.Code(err) != codes.Internal {
		t.Errorf("stream panic: %v", err)
	}

	// Domain errors are mapped; the request id comes from metadata or is generated.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(RequestIDHeader, "req-123"))
	var seen string
	_, err = s.unaryInterceptor(ctx, nil, info, func(ctx context.Context, _ any) (any, error) {
		seen = RequestID(ctx)
		return nil, fmt.Errorf("wrapped: %w", repo.ErrNotFound)
	})
	if status.Code(err) != codes.NotFound || seen != "req-123" {
		t.Errorf("err %v, request id %q", err, seen)
	}
	_, _ = s.unaryInterceptor(context.Background(), nil, info, func(ctx context.Context, _ any) (any, error) {
		seen = RequestID(ctx)
		return nil, nil
	})
	if len(seen) != 16 {
		t.Errorf("generated request id %q", seen)
	}

	// With auth on, a call without a token is rejected.
	tokens, err := auth.ParseTokens([]byte("[[token]]\nidentity = \"A <a@x>\"\nroles = [\"read\"]\ntoken = \"0123456789abcdef\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	denied := New(Config{Auth: tokens})
	called := false
	_, err = denied.unaryInterceptor(context.Background(), nil, info, func(context.Context, any) (any, error) { called = true; return nil, nil })
	if status.Code(err) != codes.Unauthenticated || called {
		t.Errorf("auth: %v, handler called %v", err, called)
	}
	if err := denied.streamInterceptor(nil, &fakeStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/x"},
		func(any, grpc.ServerStream) error { called = true; return nil }); status.Code(err) != codes.Unauthenticated || called {
		t.Errorf("stream auth: %v", err)
	}
}

func TestHTTPHandler(t *testing.T) {
	t.Parallel()
	ready := errors.New("db down")
	s := New(Config{Ready: func(context.Context) error { return ready }})
	h := s.HTTPHandler()
	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}
	if code, _ := get("/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d", code)
	}
	if code, body := get("/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "db down") {
		t.Errorf("/readyz while not ready = %d %q", code, body)
	}
	ready = nil
	if code, _ := get("/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d", code)
	}
	_, _ = s.unaryInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/m"}, func(context.Context, any) (any, error) { return nil, nil })
	if code, body := get("/metrics"); code != http.StatusOK || !strings.Contains(body, `xylem_rpc_duration_seconds_count{code="OK",method="/m"} 1`) {
		t.Errorf("/metrics = %d, missing histogram:\n%s", code, body)
	}
}
