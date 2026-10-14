// Command xy is the Xylem client CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/client"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCmd().ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "xy:", err)
	}
	os.Exit(client.ExitCode(err))
}

// globals are the persistent flags (06 §Commands).
type globals struct {
	dir     string // -C
	server  string
	repo    string
	json    bool
	verbose bool
}

func newRootCmd() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:           "xy",
		Short:         "Xylem client: workspaces, checkout, commit, land and history queries",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	f := root.PersistentFlags()
	f.StringVarP(&g.dir, "dir", "C", "", "run as if started in this directory")
	f.StringVar(&g.server, "server", "", "server address (host:port); default from the workspace")
	f.StringVar(&g.repo, "repo", "", "repository; default from the workspace")
	f.BoolVar(&g.json, "json", false, "machine-readable JSON output")
	f.BoolVarP(&g.verbose, "verbose", "v", false, "verbose logging to stderr")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return client.Usage(err) })

	root.AddCommand(
		newVersionCmd(),
		newCloneCmd(g), newStatusCmd(g), newDiffCmd(g), newCheckoutCmd(g), newSparseCmd(g), newCacheCmd(g),
		newLogCmd(g), newShowCmd(g), newBlameCmd(g), newCatCmd(g), newLsCmd(g),
		m6("add <paths>", "Mark untracked files for the next commit"),
		m6("commit", "Create a draft changeset from the working copy"),
		m6("stack", "Show draft changesets between @ and the mainline"),
		m6("sync", "Rebase or move the working copy onto main"),
		m6("land", "Land the current stack onto main"),
	)
	return root
}

// args wraps a cobra argument validator so violations exit with code 2.
func args(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		if err := v(cmd, a); err != nil {
			return client.Usage(err)
		}
		return nil
	}
}

func notYet(milestone string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		return errors.New(cmd.Name() + ": not yet implemented (" + milestone + ")")
	}
}

// m6 is a placeholder for a write-path command (07, M6).
func m6(use, short string) *cobra.Command {
	return &cobra.Command{Use: use, Short: short + " (M6)", RunE: notYet("M6")}
}

// m5 is a placeholder for a command implemented later in M5.
func m5(use, short string) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, RunE: notYet("M5")}
}

func newCloneCmd(*globals) *cobra.Command {
	return m5("clone <server> <dir>", "Create a workspace and check out a revision (full or --sparse)")
}

func newStatusCmd(*globals) *cobra.Command {
	return m5("status", "Show changed files in the working copy")
}

func newDiffCmd(*globals) *cobra.Command {
	return m5("diff [paths]", "Diff the working copy against @")
}

func newCheckoutCmd(*globals) *cobra.Command {
	return m5("checkout <rev>", "Move the working copy to a revision")
}

func newSparseCmd(*globals) *cobra.Command { return m5("sparse", "List or change the sparse profile") }
func newCacheCmd(*globals) *cobra.Command  { return m5("cache", "Local object cache stats and gc") }
func newLogCmd(*globals) *cobra.Command    { return m5("log [path]", "First-parent history") }
func newShowCmd(*globals) *cobra.Command   { return m5("show [rev]", "Changeset metadata and diff") }

func newBlameCmd(*globals) *cobra.Command { return m5("blame <path>", "Line-by-line origin of a file") }
func newCatCmd(*globals) *cobra.Command   { return m5("cat <path>", "Print a file at a revision") }
func newLsCmd(*globals) *cobra.Command    { return m5("ls [path]", "List a directory at a revision") }
