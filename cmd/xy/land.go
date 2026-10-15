package main

import (
	"errors"
	"io"
	"slices"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/client"
)

func newStackCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "stack",
		Short: "Show the draft changesets between @ and the mainline (newest first)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := g.workspace()
			if err != nil {
				return err
			}
			conn, f, err := g.fetcher(w)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			stack, err := w.Stack(cmd.Context(), f)
			if err != nil {
				return err
			}
			slices.Reverse(stack)
			if stack == nil {
				stack = []client.ChangesetInfo{}
			}
			return g.emit(cmd, stack, func(o io.Writer) error {
				for _, c := range stack {
					if err := printf(o, "%s %s\n", c.ID[:12], c.Subject()); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}

// moveCmd runs sync or land and prints the outcome, or the conflict table.
func (g *globals) moveCmd(cmd *cobra.Command, verb string, move func(w *client.Workspace, f *client.Fetcher) (*client.MoveResult, error)) error {
	return g.locked(func(w *client.Workspace, f *client.Fetcher) error {
		res, err := move(w, f)
		var conflict *client.ConflictError
		if errors.As(err, &conflict) {
			out := struct {
				Conflicting string            `json:"conflicting_changeset"`
				Conflicts   []client.Conflict `json:"conflicts"`
			}{conflict.Changeset, conflict.Conflicts}
			if perr := g.emit(cmd, out, func(o io.Writer) error {
				return printf(o, "conflicts in %s:\n%s", conflict.Changeset[:12], conflict.Table())
			}); perr != nil {
				return perr
			}
			return err
		}
		if err != nil {
			return err
		}
		moved := make([]string, len(res.Moved))
		for i, id := range res.Moved {
			moved[i] = id.String()
		}
		out := struct {
			Changeset string   `json:"changeset"`
			Moved     []string `json:"changesets"`
			Skipped   int      `json:"skipped"`
			Attempts  int      `json:"attempts,omitempty"`
			Written   int      `json:"written"`
			Deleted   int      `json:"deleted"`
		}{res.Top.String(), moved, res.Skipped, res.Attempts, res.Checkout.Written, res.Checkout.Deleted}
		return g.emit(cmd, out, func(o io.Writer) error {
			if res.Stack == 0 {
				return printf(o, "@ at %s (main)\n", res.Top.Short(12))
			}
			noun := "changesets"
			if len(res.Moved) == 1 {
				noun = "changeset"
			}
			return printf(o, "%s %d %s (%d already on main); @ at %s\n", verb, len(res.Moved), noun, res.Skipped, res.Top.Short(12))
		})
	})
}

func newSyncCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Move the working copy onto main, rebasing drafts (refuses with local changes)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return g.moveCmd(cmd, "rebased", func(w *client.Workspace, f *client.Fetcher) (*client.MoveResult, error) {
				return w.Sync(cmd.Context(), f)
			})
		},
	}
}

func newLandCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "land",
		Short: "Land the draft stack onto main and check out the new tip (exit 3 on conflicts)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return g.moveCmd(cmd, "landed", func(w *client.Workspace, f *client.Fetcher) (*client.MoveResult, error) {
				return w.Land(cmd.Context(), f)
			})
		},
	}
}
