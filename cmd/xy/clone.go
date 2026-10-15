package main

import (
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/client"
)

func newCloneCmd(g *globals) *cobra.Command {
	var rev, sparse string
	var noCache bool
	cmd := &cobra.Command{
		Use:   "clone <server> <dir>",
		Short: "Create a workspace and check out a revision (full or --sparse)",
		Args:  args(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, a []string) error {
			repo := g.repo
			var dirs []string
			if sparse != "" {
				dirs = strings.Split(sparse, ",")
			}
			res, err := client.Clone(cmd.Context(), a[0], a[1], client.CloneOptions{Repo: repo, Rev: rev, Sparse: dirs, NoCache: noCache})
			if err != nil {
				return err
			}
			out := struct {
				Dir       string `json:"dir"`
				Changeset string `json:"changeset"`
				Files     int    `json:"files"`
				Trees     int64  `json:"trees_fetched"`
				Blobs     int64  `json:"blobs_fetched"`
				Bytes     int64  `json:"bytes_fetched"`
			}{res.Workspace.Root, res.Changeset.String(), res.Files, res.Fetched.Trees, res.Fetched.Blobs, res.Fetched.Bytes}
			return g.emit(cmd, out, func(w io.Writer) error {
				return printf(w, "cloned %s at %s into %s (%d files)\n", res.Workspace.Config.Repo, res.Changeset.Short(12), res.Workspace.Root, res.Files)
			})
		},
	}
	cmd.Flags().StringVar(&rev, "rev", "main", "revision to check out")
	cmd.Flags().StringVar(&sparse, "sparse", "", "comma-separated directories to check out (default: everything)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "keep no local object cache (cache_max_bytes = 0); use with a daemon (unix://…)")
	return cmd
}
