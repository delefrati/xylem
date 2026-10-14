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

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "xylemd",
		Short:        "Xylem server, cache tiers and admin tools",
		SilenceUsage: true,
	}
	root.AddCommand(newVersionCmd())
	return root
}
