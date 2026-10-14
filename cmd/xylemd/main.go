// Command xylemd runs the Xylem server, cache and daemon tiers, the git importer and admin tools.
package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
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
	root.AddCommand(newVersionCmd(), newMigrateCmd(&g), newAdminCmd(&g), newVerifyCmd(&g))
	return root
}
