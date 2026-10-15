package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/parity"
	"github.com/delefrati/xylem/internal/server"
)

func newParityCmd(_ *globalFlags) *cobra.Command {
	var (
		gitDir, addr, repoName, branch, checks, report string
		seed                                           uint64
		commits                                        int
		blameThreshold                                 float64
	)
	cmd := &cobra.Command{
		Use:   "parity --git <dir> --server <addr>",
		Short: "Compare a running server's answers with git on the imported repository",
		Long: "Runs the parity checks of 10-testing.md (" + strings.Join(parity.AllChecks, ", ") + ")\n" +
			"against a server that imported the git repository, prints a table and writes a JSON report.\n" +
			"Exits non-zero if any check is below its threshold.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if gitDir == "" {
				return errors.New("--git is required")
			}
			src, err := gitsource.Open(ctx, gitDir, gitsource.Options{})
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()
			conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(server.MaxMessageSize)))
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			opts := parity.Options{Repo: repoName, Branch: branch, Seed: seed, Commits: commits, BlameThreshold: blameThreshold}
			if checks != "" {
				opts.Checks = strings.Split(checks, ",")
			}
			rep, err := parity.Run(ctx, src, parity.Clients{Repo: xylemv1.NewRepoServiceClient(conn), Object: xylemv1.NewObjectServiceClient(conn), Addr: addr}, opts)
			if err != nil {
				return err
			}
			if err := rep.Print(cmd.OutOrStdout()); err != nil {
				return err
			}
			if report != "" {
				if err := rep.WriteJSON(report); err != nil {
					return err
				}
			}
			if !rep.Pass() {
				return fmt.Errorf("parity failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&gitDir, "git", "", "git repository that was imported")
	cmd.Flags().StringVar(&addr, "server", "localhost:7400", "Xylem server address")
	cmd.Flags().StringVar(&repoName, "repo", "default", "Xylem repository")
	cmd.Flags().StringVar(&branch, "branch", "main", "imported git branch")
	cmd.Flags().Uint64Var(&seed, "seed", 42, "sampling seed")
	cmd.Flags().StringVar(&checks, "checks", "", "comma-separated subset of checks (default all)")
	cmd.Flags().IntVar(&commits, "commits", 100, "commits sampled by per-commit checks (0: all)")
	cmd.Flags().Float64Var(&blameThreshold, "blame-threshold", 99, "minimum % of blame lines that must match")
	cmd.Flags().StringVar(&report, "report", "parity-report.json", "JSON report path (empty: none)")
	return cmd
}
