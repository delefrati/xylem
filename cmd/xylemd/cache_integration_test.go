//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/cache"
	"github.com/delefrati/xylem/internal/server/servertest"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// startCmd runs xylemd args until the test ends and waits for /healthz.
func startCmd(t *testing.T, httpAddr string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append(args, "--log-level", "warn"))
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("xylemd %s: %v\n%s", args[0], err, out.String())
		}
	})
	for range 100 {
		if resp, err := http.Get("http://" + httpAddr + "/healthz"); err == nil { //nolint:gosec // G107: test URL
			_ = resp.Body.Close()
			return
		}
		select {
		case err := <-done:
			t.Fatalf("xylemd %s exited: %v\n%s", args[0], err, out.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("xylemd %s did not come up", args[0])
}

func TestCacheCommand(t *testing.T) {
	t.Parallel()
	s := servertest.Start(t, "tiny")
	if _, err := run(t, "cache"); err == nil || !strings.Contains(err.Error(), "--upstream is required") {
		t.Errorf("without --upstream: %v", err)
	}
	if _, err := run(t, "cache", "--upstream", freeAddr(t)); err == nil {
		t.Error("an unreachable upstream was accepted")
	}

	grpcAddr, httpAddr := freeAddr(t), freeAddr(t)
	startCmd(t, httpAddr, "cache", "--upstream", s.Addr, "--listen", grpcAddr, "--http-listen", httpAddr, "--disk-dir", t.TempDir(), "--disk-bytes", "100000000")
	resp, err := http.Get("http://" + httpAddr + "/readyz") //nolint:gosec // G107: test URL
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("/readyz: %v, %v", resp, err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	cc, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()
	if kind, err := cache.UpstreamKind(context.Background(), xylemv1.NewObjectServiceClient(cc)); err != nil || kind != cache.KindCache {
		t.Errorf("xylemd cache says it is %q (%v)", kind, err)
	}
	// A cache may not be the upstream of another cache.
	if _, err := run(t, "cache", "--upstream", grpcAddr, "--listen", freeAddr(t), "--http-listen", freeAddr(t)); err == nil || !strings.Contains(err.Error(), "upstream is a cache, want a server") {
		t.Errorf("cache → cache: %v", err)
	}
	metrics, err := http.Get(fmt.Sprintf("http://%s/metrics", httpAddr)) //nolint:gosec // G107: test URL
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = metrics.Body.Close() }()
	var body bytes.Buffer
	_, _ = body.ReadFrom(metrics.Body)
	for _, name := range []string{"xylem_cache_hits_total", "xylem_cache_misses_total", `xylem_cache_bytes{level="disk",tier="cache"}`, "xylem_upstream_inflight"} {
		if !strings.Contains(body.String(), name) {
			t.Errorf("/metrics lacks %s", name)
		}
	}
}
