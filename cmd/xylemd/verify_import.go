package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/importer"
	"github.com/delefrati/xylem/internal/repo"
)

func newVerifyImportCmd(g *globalFlags) *cobra.Command {
	var (
		dbURL, storeURL, gitDir, branch, repoName, since string
		all                                              bool
		sample                                           int
		seed                                             uint64
	)
	cmd := &cobra.Command{
		Use:   "verify-import --git <dir> (--all | --sample N [--seed S])",
		Short: "Compare imported changesets with git, tree for tree; exit non-zero on any difference",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if gitDir == "" {
				return errors.New("--git is required")
			}
			if all == (sample > 0) {
				return errors.New("give exactly one of --all or --sample N")
			}
			log, err := newLogger(g.logLevel)
			if err != nil {
				return err
			}
			d, err := openDeps(ctx, dbURL, storeURL)
			if err != nil {
				return err
			}
			defer d.Close()
			r, err := repo.Open(ctx, d.meta, d.objs, repoName, repo.Options{Logger: log})
			if err != nil {
				return err
			}
			src, err := gitsource.Open(ctx, gitDir, gitsource.Options{})
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()
			opts := importer.VerifyOptions{Branch: branch, Sample: sample, Seed: seed}
			if since != "" {
				s, err := gitsource.ParseSHA(since)
				if err != nil {
					return fmt.Errorf("--since: %w", err)
				}
				opts.Since = &s
			}
			report, err := importer.VerifyImport(ctx, r, src, opts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, v := range report.Violations {
				if _, err := fmt.Fprintln(out, "MISMATCH", v); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(out, "compared %d commits (%d files): %d mismatches\n", report.Commits, report.Files, len(report.Violations)); err != nil {
				return err
			}
			if len(report.Violations) > 0 {
				return fmt.Errorf("verify-import found %d mismatches", len(report.Violations))
			}
			return nil
		},
	}
	addDBFlag(cmd, &dbURL)
	addStoreFlag(cmd, &storeURL)
	cmd.Flags().StringVar(&gitDir, "git", "", "path to the git repository")
	cmd.Flags().StringVar(&branch, "branch", "main", "git branch that was imported")
	cmd.Flags().StringVar(&repoName, "repo", "default", "Xylem repository")
	cmd.Flags().BoolVar(&all, "all", false, "compare every commit")
	cmd.Flags().IntVar(&sample, "sample", 0, "compare N random commits")
	cmd.Flags().Uint64Var(&seed, "seed", 1, "random seed for --sample")
	cmd.Flags().StringVar(&since, "since", "", "only commits not reachable from this git sha (after an incremental import)")
	return cmd
}
