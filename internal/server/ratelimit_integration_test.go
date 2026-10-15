//go:build integration

package server_test

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/store"
)

func TestRateLimits(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	slow := server.RateLimit{Rate: 0.001, Burst: 2} // no refill within the test
	c := startServerFor(t, repotest.NewEnv(t, store.NewMem()), func(cfg *server.Config) {
		cfg.Auth, cfg.Registry = testTokens(t), reg
		cfg.RateLimits = map[string]server.RateLimit{
			server.LimitBlame: slow, server.LimitLogPath: slow, server.LimitDiffHunks: slow, server.LimitMergeBase: slow,
		}
	})
	limited := func(err error) bool { return status.Code(err) == codes.ResourceExhausted }
	diff := func(ctx context.Context, hunks bool) error {
		stream, err := c.Repo.Diff(ctx, &xylemv1.DiffRequest{ToRevision: "main", IncludeHunks: hunks})
		if err != nil {
			return err
		}
		for {
			if _, err := stream.Recv(); err != nil {
				return err
			}
		}
	}
	reader, writer := bearer(readerToken), bearer(writerToken)
	for name, call := range map[string]func(ctx context.Context, opts ...grpc.CallOption) error{
		"Blame": func(ctx context.Context, opts ...grpc.CallOption) error {
			_, err := c.Repo.Blame(ctx, &xylemv1.BlameRequest{Path: []byte("a")}, opts...)
			return err
		},
		"Log with path": func(ctx context.Context, opts ...grpc.CallOption) error {
			_, err := c.Repo.Log(ctx, &xylemv1.LogRequest{Path: []byte("a")}, opts...)
			return err
		},
		"MergeBase": func(ctx context.Context, opts ...grpc.CallOption) error {
			_, err := c.Repo.MergeBase(ctx, &xylemv1.MergeBaseRequest{A: "main", B: "main"}, opts...)
			return err
		},
		"Diff with hunks": func(ctx context.Context, _ ...grpc.CallOption) error { return diff(ctx, true) },
	} {
		for i := range 2 {
			if err := call(reader); limited(err) {
				t.Errorf("%s: call %d of the burst limited: %v", name, i+1, err)
			}
		}
		var trailer metadata.MD
		err := call(reader, grpc.Trailer(&trailer))
		if !limited(err) {
			t.Errorf("%s: third call: %v, want ResourceExhausted", name, err)
		}
		if name != "Diff with hunks" { // a stream's trailer is read through the stream
			if got := trailer.Get(server.RetryAfterTrailer); len(got) != 1 || got[0] == "" || got[0] == "0" {
				t.Errorf("%s: retry-after trailer %v", name, got)
			}
		}
		if err := call(writer); limited(err) {
			t.Errorf("%s: another identity was limited: %v", name, err)
		}
	}
	// The cheap forms are not limited.
	for range 5 {
		if _, err := c.Repo.Log(reader, &xylemv1.LogRequest{}); limited(err) {
			t.Fatalf("Log without a path limited: %v", err)
		}
		if err := diff(reader, false); limited(err) {
			t.Fatalf("Diff without hunks limited: %v", err)
		}
	}
	if v := metricValue(t, reg, "xylem_rate_limited_total"); v != 4 {
		t.Errorf("xylem_rate_limited_total = %v, want 4", v)
	}
}
