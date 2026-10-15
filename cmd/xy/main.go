// Command xy is the Xylem client CLI.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/codes"

	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/tracing"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "xy:", err)
	}
	os.Exit(client.ExitCode(err))
}

// run executes the command, traced as one span when XYLEM_OTLP_ENDPOINT is
// set (M9.5): every RPC the command makes is a child of it.
func run(ctx context.Context) error {
	shutdown, err := tracing.Setup(ctx, "xy", os.Getenv(tracing.EndpointEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "xy: tracing off:", err)
		shutdown = func(context.Context) error { return nil }
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = shutdown(flush)
	}()
	ctx, span := tracing.Tracer().Start(ctx, "xy "+commandName(os.Args[1:]))
	defer span.End()
	err = newRootCmd().ExecuteContext(ctx)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// commandName is the first argument that is not a flag (the subcommand).
func commandName(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-C" || a == "--dir" || a == "--server" || a == "--repo":
			i++ // the flag's value
		case !strings.HasPrefix(a, "-"):
			return a
		}
	}
	return ""
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
		newAddCmd(g), newCommitCmd(g),
		newStackCmd(g), newSyncCmd(g), newLandCmd(g),
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
