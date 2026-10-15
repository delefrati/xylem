package main

import (
	"io"

	"github.com/spf13/cobra"
)

func newCacheCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "cache", Short: "Local object cache stats and gc"}
	stats := &cobra.Command{
		Use:   "stats",
		Short: "Show the local object cache size",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := g.workspace()
			if err != nil {
				return err
			}
			s, err := cacheOf(w).Stats()
			if err != nil {
				return err
			}
			return g.emit(cmd, s, func(out io.Writer) error {
				if s.MaxBytes < 0 {
					return printf(out, "%d objects, %d bytes (no persistent cache)\n", s.Objects, s.Bytes)
				}
				return printf(out, "%d objects, %d bytes (limit %d)\n", s.Objects, s.Bytes, s.MaxBytes)
			})
		},
	}
	var target int64
	gc := &cobra.Command{
		Use:   "gc",
		Short: "Evict least recently used objects down to --target bytes (default 90% of the limit)",
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
			if !cmd.Flags().Changed("target") {
				target = w.Config.CacheMaxBytes / 10 * 9
			}
			removed, freed, err := cacheOf(w).GC(target)
			if err != nil {
				return err
			}
			res := struct {
				Removed int   `json:"removed"`
				Freed   int64 `json:"freed_bytes"`
			}{removed, freed}
			return g.emit(cmd, res, func(out io.Writer) error {
				return printf(out, "removed %d objects, freed %d bytes\n", removed, freed)
			})
		},
	}
	gc.Flags().Int64Var(&target, "target", 0, "evict down to this many bytes")
	cmd.AddCommand(stats, gc)
	return cmd
}
