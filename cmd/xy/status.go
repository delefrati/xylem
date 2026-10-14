package main

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/client"
)

func newStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show changed files in the working copy (M/A/D/?)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := g.workspace()
			if err != nil {
				return err
			}
			unlock, err := w.Lock()
			if err != nil {
				return err
			}
			defer unlock()
			conn, f, err := g.fetcher(w)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			st, err := w.Status(cmd.Context(), f)
			if err != nil {
				return err
			}
			type entry struct {
				Status string      `json:"status"`
				Path   client.Path `json:"path"`
			}
			out := make([]entry, len(st))
			for i, s := range st {
				out[i] = entry{string(s.Code), s.Path}
			}
			return g.emit(cmd, out, func(o io.Writer) error {
				for _, s := range st {
					if err := printf(o, "%c %s\n", s.Code, g.displayPath(w, s.Path)); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}

func newDiffCmd(g *globals) *cobra.Command {
	var contextLines int
	cmd := &cobra.Command{
		Use:   "diff [paths]",
		Short: "Diff the working copy against @ (unified, git format)",
		RunE: func(cmd *cobra.Command, a []string) error {
			w, err := g.workspace()
			if err != nil {
				return err
			}
			filter, err := g.repoPaths(w, a)
			if err != nil {
				return err
			}
			unlock, err := w.Lock()
			if err != nil {
				return err
			}
			defer unlock()
			conn, f, err := g.fetcher(w)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			return w.WriteDiff(cmd.Context(), f, cmd.OutOrStdout(), filter, contextLines)
		},
	}
	cmd.Flags().IntVarP(&contextLines, "unified", "U", 3, "lines of context")
	return cmd
}
