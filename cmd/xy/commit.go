package main

import (
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/client"
)

func newAddCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "add <paths>...",
		Short: "Mark untracked files (or every untracked file in a directory) for the next commit",
		Args:  args(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			return g.locked(func(w *client.Workspace, f *client.Fetcher) error {
				paths, err := g.repoPaths(w, a)
				if err != nil {
					return err
				}
				added, err := w.Add(cmd.Context(), f, paths)
				if err != nil {
					return err
				}
				out := make([]client.Path, len(added))
				for i, p := range added {
					out[i] = p
				}
				return g.emit(cmd, out, func(o io.Writer) error {
					for _, p := range added {
						if err := printf(o, "A %s\n", g.displayPath(w, p)); err != nil {
							return err
						}
					}
					return nil
				})
			})
		},
	}
}

func newCommitCmd(g *globals) *cobra.Command {
	var message string
	var all bool
	cmd := &cobra.Command{
		Use:   "commit -m <message>",
		Short: "Create a draft changeset on top of @ from the working copy",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return g.locked(func(w *client.Workspace, f *client.Fetcher) error {
				msg := message
				if msg != "" && !strings.HasSuffix(msg, "\n") {
					msg += "\n"
				}
				res, err := w.Commit(cmd.Context(), f, client.CommitOptions{Message: []byte(msg), All: all})
				if err != nil {
					return err
				}
				out := struct {
					Changeset     string `json:"changeset"`
					Changes       int    `json:"changes"`
					UploadedBlobs int64  `json:"uploaded_blobs"`
					UploadedBytes int64  `json:"uploaded_bytes"`
				}{res.Changeset.String(), res.Changes, res.Uploaded.Blobs, res.Uploaded.Bytes}
				return g.emit(cmd, out, func(o io.Writer) error {
					subject, _, _ := strings.Cut(msg, "\n")
					return printf(o, "%s %s\n", res.Changeset.Short(12), subject)
				})
			})
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "commit message (required)")
	cmd.Flags().BoolVarP(&all, "all", "A", false, "also commit untracked files")
	return cmd
}
