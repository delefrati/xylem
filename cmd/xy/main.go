// Command xy is the Xylem client CLI.
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

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "xy",
		Short:        "Xylem client: workspaces, checkout, commit, land and history queries",
		SilenceUsage: true,
	}
	root.AddCommand(newVersionCmd())
	return root
}
