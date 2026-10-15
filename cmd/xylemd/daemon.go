package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/cache"
)

// defaultSocket is $XDG_RUNTIME_DIR/xylem/daemon.sock (09 §Daemon specifics),
// or a per-user directory under the temp dir without XDG_RUNTIME_DIR.
func defaultSocket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "xylem", "daemon.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("xylem-%d", os.Getuid()), "daemon.sock")
}

// defaultDaemonDisk is $XDG_CACHE_HOME/xylem/objects.
func defaultDaemonDisk() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "xylem", "objects")
}

// prepareSocket creates the socket's directory (0700) and removes a stale
// socket file; a live daemon on it is an error.
func prepareSocket(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	conn, err := net.Dial("unix", path)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("a daemon is already listening on %s", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("socket %s: %w", path, err)
	}
	return os.Remove(path) // left by a daemon that died
}

func newDaemonCmd(g *globalFlags) *cobra.Command {
	var f tierFlags
	var socket, serverAddr string
	var direct bool
	cmd := &cobra.Command{
		Use:   "daemon --upstream <cache> --server <server>",
		Short: "Run the per-machine daemon: shared object cache on a Unix socket, proxying history and writes",
		Long: "Serves ObjectService on a Unix socket from memory and a disk cache, fetching misses from --upstream,\n" +
			"which must be a xylemd cache (or a server with --allow-direct-upstream). RepoService and WriteService\n" +
			"calls are proxied unchanged to --server, so clients need only the socket (server = \"unix://…\").",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			log, err := newJSONLogger(g.logLevel)
			if err != nil {
				return err
			}
			stopTracing, err := g.startTracing(ctx, "xylemd-daemon")
			if err != nil {
				return err
			}
			defer stopTracing()
			if serverAddr == "" {
				return errors.New("--server is required (where RepoService and WriteService calls go)")
			}
			upKind := cache.KindCache
			if direct {
				upKind = cache.KindServer
			}
			up, err := dialUpstream(ctx, f.upstream, upKind, f.upstreamToken())
			if err != nil {
				return err
			}
			defer func() { _ = up.Close() }()
			srv, err := dialUpstream(ctx, serverAddr, cache.KindServer, f.upstreamToken())
			if err != nil {
				return fmt.Errorf("--server: %w", err)
			}
			defer func() { _ = srv.Close() }()
			metrics := cache.NewMetrics(cache.KindDaemon)
			tier, err := f.newTier(up, metrics)
			if err != nil {
				return err
			}
			metrics.Watch(tier)
			if err := prepareSocket(socket); err != nil {
				return err
			}
			g, err := f.grpcServer(cache.NewService(tier, cache.KindDaemon, log), log, cache.Proxy(srv)...)
			if err != nil {
				return err
			}
			return serve(ctx, g, tierHTTP(metrics, up, srv), "unix", socket, f.httpListen, log)
		},
	}
	cmd.Flags().StringVar(&socket, "socket", envOr("XYLEM_SOCKET", defaultSocket()), "Unix socket to listen on (env XYLEM_SOCKET)")
	cmd.Flags().StringVar(&serverAddr, "server", envOr("XYLEM_SERVER", ""), "xylemd server for RepoService and WriteService (env XYLEM_SERVER)")
	cmd.Flags().BoolVar(&direct, "allow-direct-upstream", false, "allow --upstream to be a server instead of a cache (development)")
	f.add(cmd, 256<<20, defaultDaemonDisk(), 20<<30, "127.0.0.1:7402")
	return cmd
}
