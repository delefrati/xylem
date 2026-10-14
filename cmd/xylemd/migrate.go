package main

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/migrations"
)

func newMigrateCmd(g *globalFlags) *cobra.Command {
	var dbURL string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log, err := newLogger(g.logLevel)
			if err != nil {
				return err
			}
			pool, err := pgxpool.New(cmd.Context(), dbURL)
			if err != nil {
				return fmt.Errorf("connecting to database: %w", err)
			}
			defer pool.Close()
			applied, err := meta.Migrate(cmd.Context(), pool, migrations.FS, log)
			if err != nil {
				return err
			}
			if len(applied) == 0 {
				log.Info("database schema is up to date")
			}
			return nil
		},
	}
	addDBFlag(cmd, &dbURL)
	return cmd
}
