// Command xylemd runs the Xylem server, cache and daemon tiers, the git importer and admin tools.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
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
}

func newRootCmd() *cobra.Command {
	var g globalFlags
	root := &cobra.Command{
		Use:          "xylemd",
		Short:        "Xylem server, cache tiers and admin tools",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&g.logLevel, "log-level", envOr("XYLEM_LOG_LEVEL", "info"), "debug, info, warn or error (env XYLEM_LOG_LEVEL)")
	root.AddCommand(newVersionCmd(), newMigrateCmd(&g), newAdminCmd(&g), newVerifyCmd(&g), newImportCmd(&g), newVerifyImportCmd(&g), newServerCmd(&g), newCacheCmd(&g), newParityCmd(&g))
	return root
}
