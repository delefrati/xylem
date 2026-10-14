package main

import (
	"context"
	"errors"
	"fmt"
	"os/user"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/store"
)

// deps are the stores an admin command works on.
type deps struct {
	pool *pgxpool.Pool
	meta *meta.PG
	objs store.ObjectStore
}

func openDeps(ctx context.Context, dbURL, storeURL string) (*deps, error) {
	objs, err := store.Open(storeURL)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	return &deps{pool: pool, meta: meta.NewPG(pool), objs: objs}, nil
}

func (d *deps) Close() { d.pool.Close() }

// actor names the person running an admin command, for the ref log.
func actor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "admin:" + u.Username
	}
	return "admin"
}

func newAdminCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Repository and ref administration",
	}
	cmd.AddCommand(newInitRepoCmd(g), newSetRefCmd(g))
	return cmd
}

func newInitRepoCmd(g *globalFlags) *cobra.Command {
	var dbURL, storeURL string
	cmd := &cobra.Command{
		Use:   "init-repo <name>",
		Short: "Create a repository with an empty root changeset and a main ref",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log, err := newLogger(g.logLevel)
			if err != nil {
				return err
			}
			d, err := openDeps(cmd.Context(), dbURL, storeURL)
			if err != nil {
				return err
			}
			defer d.Close()
			_, root, err := repo.Init(cmd.Context(), d.meta, d.objs, args[0], repo.Options{Logger: log})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "created repo %s; %s -> %s\n", args[0], repo.MainRef, root.ID)
			return err
		},
	}
	addDBFlag(cmd, &dbURL)
	addStoreFlag(cmd, &storeURL)
	return cmd
}

func newSetRefCmd(g *globalFlags) *cobra.Command {
	var dbURL, storeURL, expectedHex string
	var force bool
	cmd := &cobra.Command{
		Use:   "set-ref <repo> <ref> <changeset>",
		Short: "Point a ref at a changeset (fast-forward unless --force)",
		Long: "Point a ref at a changeset, given as a full id or a unique prefix of at least 8 hex characters.\n" +
			"The ref is updated by compare-and-swap from --expected (default: its current value), or created\n" +
			"if it does not exist. Without --force the update must be a first-parent fast-forward.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
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
			target, err := r.ResolveHex(ctx, args[2])
			if err != nil {
				return err
			}
			var expected hash.ID
			switch {
			case expectedHex != "":
				if expected, err = r.ResolveHex(ctx, expectedHex); err != nil {
					return fmt.Errorf("--expected: %w", err)
				}
			default:
				cur, err := r.GetRef(ctx, args[1])
				if err == nil {
					expected = cur.ID
				} else if !isNotFound(err) {
					return err
				}
			}
			ref, err := r.UpdateRef(ctx, args[1], expected, target, force, actor(), "admin")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s -> %s (version %d)\n", ref.Name, ref.ID, ref.Version)
			return err
		},
	}
	addDBFlag(cmd, &dbURL)
	addStoreFlag(cmd, &storeURL)
	cmd.Flags().BoolVar(&force, "force", false, "allow a non-fast-forward update (rewinds the mainline)")
	cmd.Flags().StringVar(&expectedHex, "expected", "", "expected current value (default: read the ref)")
	return cmd
}

func isNotFound(err error) bool { return errors.Is(err, repo.ErrNotFound) }
