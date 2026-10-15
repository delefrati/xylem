// Command xylemd runs the Xylem server, cache and daemon tiers, the git importer and admin tools.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/tracing"
)

func main() {
	// Interrupts cancel the context, so long commands (import) stop cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCmd().ExecuteContext(ctx)
	stop()
	if err != nil {
		os.Exit(1)
	}
}

// globalFlags are persistent flags shared by all subcommands.
type globalFlags struct {
	logLevel string
	otlp     string // OTLP endpoint for traces; empty = off
}

// startTracing turns tracing on for a long-running command (server, cache,
// daemon) when --otlp-endpoint is set; call the result when it stops.
func (g *globalFlags) startTracing(ctx context.Context, service string) (stop func(), err error) {
	shutdown, err := tracing.Setup(ctx, service, g.otlp)
	if err != nil {
		return nil, err
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx) // flush what is buffered; tracing is best effort
	}, nil
}

func newRootCmd() *cobra.Command {
	var g globalFlags
	root := &cobra.Command{
		Use:          "xylemd",
		Short:        "Xylem server, cache tiers and admin tools",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&g.logLevel, "log-level", envOr("XYLEM_LOG_LEVEL", "info"), "debug, info, warn or error (env XYLEM_LOG_LEVEL)")
	root.PersistentFlags().StringVar(&g.otlp, "otlp-endpoint", envOr(tracing.EndpointEnv, ""),
		"send OpenTelemetry traces over OTLP/gRPC to host:port or http(s)://host:port (server, cache, daemon); empty = off (env "+tracing.EndpointEnv+")")
	root.AddCommand(newVersionCmd(), newMigrateCmd(&g), newAdminCmd(&g), newVerifyCmd(&g), newImportCmd(&g), newVerifyImportCmd(&g), newServerCmd(&g), newCacheCmd(&g), newDaemonCmd(&g), newParityCmd(&g), newGCCmd(&g))
	return root
}
