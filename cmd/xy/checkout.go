package main

import (
	"errors"
	"io"

	"github.com/spf13/cobra"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/client"
)

// checkoutOutput is the --json result of checkout and sparse add/remove.
type checkoutOutput struct {
	Changeset string        `json:"changeset"`
	Sparse    []client.Path `json:"sparse"`
	Written   int           `json:"written"`
	Deleted   int           `json:"deleted"`
	Trees     int64         `json:"trees_fetched"`
	Blobs     int64         `json:"blobs_fetched"`
	Bytes     int64         `json:"bytes_fetched"`
}

func (g *globals) emitCheckout(cmd *cobra.Command, w *client.Workspace, res *client.CheckoutResult) error {
	out := checkoutOutput{
		w.State.Parent.String(), w.State.Sparse, res.Written, res.Deleted,
		res.Fetched.Trees, res.Fetched.Blobs, res.Fetched.Bytes,
	}
	return g.emit(cmd, out, func(o io.Writer) error {
		return printf(o, "at %s (%d files written, %d deleted)\n", w.State.Parent.Short(12), res.Written, res.Deleted)
	})
}

// locked opens the workspace, takes its lock and dials its server.
func (g *globals) locked(run func(w *client.Workspace, f *client.Fetcher) error) error {
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
	return run(w, f)
}

func newCheckoutCmd(g *globals) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "checkout <rev>",
		Short: "Move the working copy to a revision (refuses to overwrite local changes)",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			return g.locked(func(w *client.Workspace, f *client.Fetcher) error {
				rev, err := client.ResolveRev(a[0], w)
				if err != nil {
					return err
				}
				resp, err := f.Conn().Repo.ResolveRevision(cmd.Context(), &xylemv1.ResolveRevisionRequest{Repo: g.repoName(w), Revision: rev})
				if err != nil {
					return err
				}
				var target client.Target
				copy(target.Changeset[:], resp.GetId())
				copy(target.Root[:], resp.GetRootTree())
				res, err := w.Checkout(cmd.Context(), f, target, force)
				if err != nil {
					return err
				}
				return g.emitCheckout(cmd, w, res)
			})
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "discard local changes to the files involved")
	return cmd
}

func newSparseCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "sparse", Short: "List or change the sparse profile"}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the sparse directories (nothing for a full checkout)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := g.workspace()
			if err != nil {
				return err
			}
			return g.emit(cmd, w.State.Sparse, func(o io.Writer) error {
				for _, d := range w.State.Sparse {
					if err := printf(o, "%s\n", client.QuotePath(d)); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	change := func(use, short string, apply func(profile, dirs []client.Path) ([]client.Path, error)) *cobra.Command {
		return &cobra.Command{
			Use:   use + " <dir>...",
			Short: short,
			Args:  args(cobra.MinimumNArgs(1)),
			RunE: func(cmd *cobra.Command, a []string) error {
				return g.locked(func(w *client.Workspace, f *client.Fetcher) error {
					rel, err := g.repoPaths(w, a)
					if err != nil {
						return err
					}
					dirs := make([]client.Path, len(rel))
					for i, d := range rel {
						if len(d) == 0 {
							return client.Usage(errors.New("the workspace root cannot be a sparse directory"))
						}
						dirs[i] = d
					}
					profile, err := apply(w.State.Sparse, dirs)
					if err != nil {
						return err
					}
					res, err := w.SetSparse(cmd.Context(), f, profile)
					if err != nil {
						return err
					}
					return g.emitCheckout(cmd, w, res)
				})
			},
		}
	}
	cmd.AddCommand(list,
		change("add", "Add directories to the profile and check them out", client.SparseAdd),
		change("remove", "Remove directories from the profile and delete their files", client.SparseRemove),
	)
	return cmd
}
