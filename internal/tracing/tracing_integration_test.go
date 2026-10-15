//go:build integration

package tracing_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/cache"
	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/server/servertest"
	"github.com/delefrati/xylem/internal/tracing"
)

// serve runs g on a new listener and returns its address.
func serve(t *testing.T, g *grpc.Server, network, addr string) string {
	t.Helper()
	lis, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	return lis.Addr().String()
}

func dial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), tracing.DialOption())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

// TestTraceAcrossTiers is M9.5's check: one trace follows a call from the
// client through the daemon and the cache to the server, for proxied
// history calls and for object fetches through both tiers' batches.
func TestTraceAcrossTiers(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	shutdown := tracing.Install("test", sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	s := servertest.Start(t, "tiny")
	srv := dial(t, s.Addr)
	cacheTier := cache.NewTier(cache.Config{Upstream: xylemv1.NewObjectServiceClient(srv), MemoryBytes: 1 << 20})
	cacheAddr := serve(t, cache.NewService(cacheTier, cache.KindCache, nil).NewGRPCServer(), "tcp", "127.0.0.1:0")
	daemonTier := cache.NewTier(cache.Config{Upstream: xylemv1.NewObjectServiceClient(dial(t, cacheAddr)), MemoryBytes: 1 << 20})
	sock := filepath.Join(t.TempDir(), "d.sock")
	serve(t, cache.NewService(daemonTier, cache.KindDaemon, nil).NewGRPCServer(cache.Proxy(srv)...), "unix", sock)
	conn, err := client.Dial("unix://"+sock, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, root := tracing.Tracer().Start(context.Background(), "xy test")
	tip, err := conn.Repo.ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Revision: "main"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.Object.GetObjects(ctx, &xylemv1.GetObjectsRequest{Ids: [][]byte{tip.GetRootTree()}})
	if err != nil {
		t.Fatal(err)
	}
	if msg, err := stream.Recv(); err != nil || len(msg.GetObjects()) != 1 {
		t.Fatalf("fetch: %v, %v", msg, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) { // a stream's client span ends at EOF
		t.Fatalf("end of fetch: %v", err)
	}
	root.End()

	// Server spans end just after the response; wait for all of them.
	want := map[string]int{
		"internal xy test": 1,
		// xy → daemon, and the daemon's proxy → server.
		"client xylem.v1.RepoService/ResolveRevision": 2, "server xylem.v1.RepoService/ResolveRevision": 2,
		// xy → daemon → (daemon batch) → cache → (cache batch) → server.
		"client xylem.v1.ObjectService/GetObjects": 3, "server xylem.v1.ObjectService/GetObjects": 3,
		"internal cache.fetch": 2,
	}
	var got map[string]int
	deadline := time.Now().Add(5 * time.Second)
	for {
		got = map[string]int{}
		for _, sp := range rec.Ended() {
			if sp.SpanContext().TraceID() != root.SpanContext().TraceID() {
				continue
			}
			got[fmt.Sprintf("%s %s", sp.SpanKind(), sp.Name())]++
		}
		if equal(got, want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !equal(got, want) {
		t.Errorf("spans in the trace:\n%s\nwant:\n%s", format(got), format(want))
	}
	// Every span but the root has its parent in the trace, and each server
	// span's parent is a distinct client span: the daemon's proxy replaced
	// the forwarded trace context with its own.
	byID := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	for _, sp := range rec.Ended() {
		if sp.SpanContext().TraceID() == root.SpanContext().TraceID() {
			byID[sp.SpanContext().SpanID()] = sp
		}
	}
	serverParents := map[trace.SpanID]bool{}
	for _, sp := range byID {
		parent, ok := byID[sp.Parent().SpanID()]
		switch {
		case sp.Name() == "xy test":
		case !ok:
			t.Errorf("span %s has no parent in the trace", sp.Name())
		case sp.SpanKind() == trace.SpanKindServer:
			if parent.SpanKind() != trace.SpanKindClient || serverParents[parent.SpanContext().SpanID()] {
				t.Errorf("server span %s: parent %s %s (seen before: %v)", sp.Name(), parent.SpanKind(), parent.Name(), serverParents[parent.SpanContext().SpanID()])
			}
			serverParents[parent.SpanContext().SpanID()] = true
		}
	}
}

func equal(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

func format(m map[string]int) string {
	var lines []string
	for k, v := range m {
		lines = append(lines, fmt.Sprintf("  %d × %s", v, k))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
