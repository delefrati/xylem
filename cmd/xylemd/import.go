package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/importer"
	"github.com/delefrati/xylem/internal/repo"
)

// progressEvery is how often import logs throughput (M3.6).
const progressEvery = 5 * time.Second

func newImportCmd(g *globalFlags) *cobra.Command {
	var (
		dbURL, storeURL, gitDir, branch, repoName string
		follow                                    time.Duration
		batch, limit, workers                     int
	)
	cmd := &cobra.Command{
		Use:   "import --git <dir> [--branch main]",
		Short: "Import a git branch into a repository (resumable, incremental)",
		Long: "Imports every commit reachable from refs/heads/<branch>, parents before children, and moves\n" +
			"Xylem's main to the imported tip at each checkpoint. Re-running continues where it stopped.\n" +
			"With --follow it then polls the git repository and imports new commits until interrupted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if gitDir == "" {
				return errors.New("--git is required")
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
			r, err := repo.OpenOrCreate(ctx, d.meta, d.objs, repoName, repo.Options{Logger: log})
			if err != nil {
				return err
			}
			src, err := gitsource.Open(ctx, gitDir, gitsource.Options{})
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()
			opts := importer.Options{Branch: branch, Batch: batch, Limit: limit, Workers: workers, Logger: log}
			for {
				stats, err := importOnce(ctx, r, src, opts, log)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "imported %d new commits (%d blobs, %d bytes); %s -> %s\n",
					stats.Commits, stats.Blobs, stats.BlobBytes, repo.MainRef, stats.MainTip); err != nil {
					return err
				}
				if follow <= 0 {
					return nil
				}
				select {
				case <-ctx.Done():
					return nil // interrupted while waiting: the last run completed
				case <-time.After(follow):
				}
			}
		},
	}
	addDBFlag(cmd, &dbURL)
	addStoreFlag(cmd, &storeURL)
	cmd.Flags().StringVar(&gitDir, "git", "", "path to the git repository (bare or not)")
	cmd.Flags().StringVar(&branch, "branch", "main", "git branch to import")
	cmd.Flags().StringVar(&repoName, "repo", "default", "Xylem repository (created if missing)")
	cmd.Flags().DurationVar(&follow, "follow", 0, "after importing, poll for new commits at this interval (e.g. 30s)")
	cmd.Flags().IntVar(&batch, "batch", 500, "commits between checkpoints")
	cmd.Flags().IntVar(&limit, "limit", 0, "import at most N new commits (0: all)")
	cmd.Flags().IntVar(&workers, "workers", 8, "parallel commit readers and blob uploaders")
	return cmd
}

// importOnce runs one import, logging throughput every progressEvery.
func importOnce(ctx context.Context, r *repo.Repo, src *gitsource.Repo, opts importer.Options, log *slog.Logger) (importer.Stats, error) {
	im, err := importer.New(r, src, opts)
	if err != nil {
		return importer.Stats{}, err
	}
	start := time.Now()
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(progressEvery)
		defer ticker.Stop()
		prev, prevAt := im.Stats(), start
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				s := im.Stats()
				secs := now.Sub(prevAt).Seconds()
				log.Info("import progress",
					"commits", s.Commits, "commits_per_s", rate(s.Commits-prev.Commits, secs),
					"blobs", s.Blobs, "blobs_per_s", rate(s.Blobs-prev.Blobs, secs), "bytes", s.BlobBytes)
				prev, prevAt = s, now
			}
		}
	}()
	stats, err := im.Run(ctx)
	close(done)
	elapsed := time.Since(start)
	log.Info("import finished", "commits", stats.Commits, "blobs", stats.Blobs, "bytes", stats.BlobBytes,
		"elapsed", elapsed.Round(time.Millisecond), "commits_per_s", rate(stats.Commits, elapsed.Seconds()), "error", err)
	return stats, err
}

func rate(n int64, secs float64) string {
	if secs <= 0 {
		return "0"
	}
	return fmt.Sprintf("%.0f", float64(n)/secs)
}
