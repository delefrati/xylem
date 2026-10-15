package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/treeops"
)

// drainTimeout bounds graceful shutdown (M4.1).
const drainTimeout = 10 * time.Second

func newServerCmd(g *globalFlags) *cobra.Command {
	var (
		dbURL, storeURL, listen, httpListen string
		objectCacheBytes, mergeMaxBytes     int64
	)
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the gRPC server (RepoService, ObjectService, WriteService) and the health/metrics endpoint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			log, err := newJSONLogger(g.logLevel)
			if err != nil {
				return err
			}
			d, err := openDeps(ctx, dbURL, storeURL)
			if err != nil {
				return err
			}
			defer d.Close()
			srv := server.New(server.Config{
				Meta: d.meta, Objects: d.objs, Logger: log, ObjectCacheBytes: objectCacheBytes,
				RepoOptions: repo.Options{MergeMaxBytes: mergeMaxBytes},
				Ready: func(ctx context.Context) error {
					if err := d.meta.Ping(ctx); err != nil {
						return fmt.Errorf("postgres: %w", err)
					}
					if _, err := d.objs.Has(ctx, []hash.ID{treeops.EmptyTree}); err != nil {
						return fmt.Errorf("object store: %w", err)
					}
					return nil
				},
			})
			return serve(ctx, srv.NewGRPCServer(), srv.HTTPHandler(), "tcp", listen, httpListen, log)
		},
	}
	addDBFlag(cmd, &dbURL)
	addStoreFlag(cmd, &storeURL)
	cmd.Flags().StringVar(&listen, "listen", envOr("XYLEM_LISTEN", ":7400"), "gRPC address (env XYLEM_LISTEN)")
	cmd.Flags().StringVar(&httpListen, "http-listen", envOr("XYLEM_HTTP_LISTEN", ":7401"), "HTTP address for /healthz, /readyz, /metrics (env XYLEM_HTTP_LISTEN)")
	cmd.Flags().Int64Var(&mergeMaxBytes, "merge-max-bytes", envInt64("XYLEM_MERGE_MAX_BYTES", repo.DefaultMergeMaxBytes),
		"largest file side rebase merges line by line; larger ones conflict as a whole (env XYLEM_MERGE_MAX_BYTES)")
	cmd.Flags().Int64Var(&objectCacheBytes, "object-cache-bytes", envInt64("XYLEM_OBJECT_CACHE_BYTES", 1<<30), "in-memory object cache size (env XYLEM_OBJECT_CACHE_BYTES)")
	return cmd
}

// serve runs the gRPC server (on network "tcp" or "unix") and the HTTP
// server until ctx is done, then drains.
func serve(ctx context.Context, g *grpc.Server, handler http.Handler, network, listen, httpListen string, log *slog.Logger) error {
	lc := net.ListenConfig{}
	grpcLis, err := lc.Listen(ctx, network, listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", listen, err)
	}
	httpLis, err := lc.Listen(ctx, "tcp", httpListen)
	if err != nil {
		_ = grpcLis.Close()
		return fmt.Errorf("listening on %s: %w", httpListen, err)
	}
	h := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- g.Serve(grpcLis) }()
	go func() {
		if err := h.Serve(httpLis); !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()
	log.Info("listening", "grpc", grpcLis.Addr().String(), "http", httpLis.Addr().String())

	select {
	case <-ctx.Done():
	case err := <-errs:
		g.Stop()
		_ = h.Close()
		return err
	}
	log.Info("shutting down", "drain", drainTimeout.String())
	stopped := make(chan struct{})
	go func() { g.GracefulStop(); close(stopped) }()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	_ = h.Shutdown(shutdownCtx)
	select {
	case <-stopped:
	case <-shutdownCtx.Done():
		log.Warn("drain timeout; closing remaining connections")
		g.Stop()
	}
	return nil
}

// newJSONLogger returns the server's JSON logger on stderr.
func newJSONLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		return nil, fmt.Errorf("--log-level %q: %w", level, err)
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}
