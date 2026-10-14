package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Set at link time: -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = ""
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit and Go version",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "xy %s (commit %s, %s %s/%s)\n",
				version, buildCommit(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return err
		},
	}
}

// buildCommit returns the link-time commit, falling back to the VCS stamp
// that go build embeds when run inside a git checkout.
func buildCommit() string {
	if commit != "" {
		return commit
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
