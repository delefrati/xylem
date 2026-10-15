package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/auth"
	"github.com/delefrati/xylem/internal/cache"
	"github.com/delefrati/xylem/internal/tracing"
)

// upstreamCheckTimeout bounds the startup check of a tier's upstream.
const upstreamCheckTimeout = 10 * time.Second

// tierFlags are the flags shared by the cache and daemon tiers.
type tierFlags struct {
	upstream, diskDir, httpListen string
	token, authTokens             string
	memoryBytes, diskBytes        int64
	maxInflight                   int
}

func (f *tierFlags) add(cmd *cobra.Command, memoryDefault int64, diskDirDefault string, diskDefault int64, httpDefault string) {
	cmd.Flags().StringVar(&f.upstream, "upstream", envOr("XYLEM_UPSTREAM", ""), "upstream tier address host:port (env XYLEM_UPSTREAM)")
	cmd.Flags().Int64Var(&f.memoryBytes, "memory-bytes", envInt64("XYLEM_MEMORY_BYTES", memoryDefault), "memory cache size (env XYLEM_MEMORY_BYTES)")
	cmd.Flags().StringVar(&f.diskDir, "disk-dir", envOr("XYLEM_DISK_DIR", diskDirDefault), "disk cache directory; empty disables it (env XYLEM_DISK_DIR)")
	cmd.Flags().Int64Var(&f.diskBytes, "disk-bytes", envInt64("XYLEM_DISK_BYTES", diskDefault), "disk cache size (env XYLEM_DISK_BYTES)")
	cmd.Flags().IntVar(&f.maxInflight, "max-inflight", cache.DefaultMaxInflight, "concurrent upstream requests; more queue, then fail fast")
	cmd.Flags().StringVar(&f.httpListen, "http-listen", envOr("XYLEM_HTTP_LISTEN", httpDefault), "HTTP address for /healthz, /readyz, /metrics (env XYLEM_HTTP_LISTEN)")
	cmd.Flags().StringVar(&f.token, "token", "", "token for upstream calls; prefer the XYLEM_TOKEN env (a flag shows in ps)")
	addAuthTokensFlag(cmd, &f.authTokens)
}

// upstreamToken is --token, else XYLEM_TOKEN.
func (f *tierFlags) upstreamToken() string {
	if f.token != "" {
		return f.token
	}
	return os.Getenv("XYLEM_TOKEN")
}

// grpcServer returns the tier's gRPC server, enforcing --auth-tokens.
func (f *tierFlags) grpcServer(svc *cache.Service, log *slog.Logger, extra ...grpc.ServerOption) (*grpc.Server, error) {
	if f.authTokens == "" {
		log.Info("no --auth-tokens: clients are not authenticated here (upstream calls still carry their token or --token)")
		return svc.NewGRPCServer(extra...), nil
	}
	tokens, err := auth.LoadTokens(f.authTokens)
	if err != nil {
		return nil, err
	}
	return svc.NewGRPCServer(append(auth.ServerInterceptors(tokens), extra...)...), nil
}

// dialUpstream connects to an upstream tier, sending token on calls that do
// not forward a client's, and checks its kind.
func dialUpstream(ctx context.Context, addr, want, token string) (*grpc.ClientConn, error) {
	if addr == "" {
		return nil, errors.New("--upstream is required")
	}
	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(cache.MaxMessageSize), grpc.MaxCallSendMsgSize(cache.MaxMessageSize)),
		tracing.DialOption(),
	}, auth.DialOptions(token)...)
	cc, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: %w", addr, err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, upstreamCheckTimeout)
	defer cancel()
	if err := cache.CheckUpstream(checkCtx, xylemv1.NewObjectServiceClient(cc), want); err != nil {
		_ = cc.Close()
		return nil, fmt.Errorf("upstream %s: %w", addr, err)
	}
	return cc, nil
}

// newTier builds the tier's cache over upstream.
func (f *tierFlags) newTier(upstream *grpc.ClientConn, observe cache.Observer) (*cache.Tier, error) {
	cfg := cache.Config{
		Upstream: xylemv1.NewObjectServiceClient(upstream), MemoryBytes: f.memoryBytes,
		MaxInflight: f.maxInflight, Observe: observe,
	}
	if f.diskDir != "" {
		disk, err := cache.OpenDisk(f.diskDir, f.diskBytes)
		if err != nil {
			return nil, err
		}
		cfg.Disk = disk
	}
	return cache.NewTier(cfg), nil
}

func newCacheCmd(g *globalFlags) *cobra.Command {
	var f tierFlags
	var listen string
	cmd := &cobra.Command{
		Use:   "cache --upstream <server>",
		Short: "Run a regional cache tier: ObjectService only, misses fetched from a server",
		Long: "Serves ObjectService (GetObjects, HasObjects, PutObjects written through) from memory and an optional\n" +
			"disk cache, fetching misses from --upstream, which must be a xylemd server (09: daemon → cache → server).\n" +
			"RepoService and WriteService return Unimplemented: point clients at a daemon or the server for those.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			log, err := newJSONLogger(g.logLevel)
			if err != nil {
				return err
			}
			stopTracing, err := g.startTracing(ctx, "xylemd-cache")
			if err != nil {
				return err
			}
			defer stopTracing()
			up, err := dialUpstream(ctx, f.upstream, cache.KindServer, f.upstreamToken())
			if err != nil {
				return err
			}
			defer func() { _ = up.Close() }()
			metrics := cache.NewMetrics(cache.KindCache)
			tier, err := f.newTier(up, metrics)
			if err != nil {
				return err
			}
			metrics.Watch(tier)
			g, err := f.grpcServer(cache.NewService(tier, cache.KindCache, log), log)
			if err != nil {
				return err
			}
			return serve(ctx, g, tierHTTP(metrics, up), "tcp", listen, f.httpListen, log)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", envOr("XYLEM_LISTEN", ":7400"), "gRPC address (env XYLEM_LISTEN)")
	f.add(cmd, 4<<30, "", 100<<30, ":7401")
	return cmd
}

// tierHTTP serves /healthz, /readyz (every upstream reachable) and /metrics.
func tierHTTP(metrics *cache.Metrics, upstreams ...*grpc.ClientConn) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for _, up := range upstreams {
			if _, err := cache.UpstreamKind(ctx, xylemv1.NewObjectServiceClient(up)); err != nil {
				http.Error(w, "upstream "+up.Target()+": "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.Handle("/metrics", metrics.Handler())
	return mux
}
