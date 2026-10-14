package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/repo"
)

func newVerifyCmd(g *globalFlags) *cobra.Command {
	var (
		dbURL, storeURL string
		all, objects    bool
		sample          int
	)
	cmd := &cobra.Command{
		Use:   "verify <repo>",
		Short: "Recompute and cross-check stored history; exit non-zero on any violation",
		Long: "Checks every changeset (or --sample N random ones): body hash and columns, the root tree\n" +
			"re-derived from the parent and the changes, core derived fields, seq ordering, and that the\n" +
			"mainline of main equals the first-parent walk from its tip. --objects also checks that the\n" +
			"blobs and trees each changeset introduces exist in the object store.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if all && sample > 0 {
				return errors.New("--all and --sample are mutually exclusive")
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
			r, err := repo.Open(ctx, d.meta, d.objs, args[0], repo.Options{Logger: log})
			if err != nil {
				return err
			}
			report, err := r.Verify(ctx, repo.VerifyOptions{
				Sample:  sample,
				Objects: objects,
				Progress: func(n int) {
					if n%10_000 < 500 {
						log.Info("verifying", "changesets", n)
					}
				},
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, v := range report.Violations {
				if _, err := fmt.Fprintln(out, "VIOLATION", v); err != nil {
					return err
				}
			}
			more := ""
			if report.Truncated {
				more = " (truncated)"
			}
			if _, err := fmt.Fprintf(out, "checked %d changesets and %d refs: %d violations%s\n",
				report.Changesets, report.Refs, len(report.Violations), more); err != nil {
				return err
			}
			if len(report.Violations) > 0 {
				return fmt.Errorf("verify found %d violations", len(report.Violations))
			}
			return nil
		},
	}
	addDBFlag(cmd, &dbURL)
	addStoreFlag(cmd, &storeURL)
	cmd.Flags().BoolVar(&all, "all", false, "check every changeset (the default)")
	cmd.Flags().IntVar(&sample, "sample", 0, "check N random changesets instead of all")
	cmd.Flags().BoolVar(&objects, "objects", false, "also check that referenced blobs and trees exist")
	return cmd
}
