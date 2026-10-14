// Package migrations embeds the numbered SQL migration files
// (NNNN_name.sql). They are applied in order by meta.Migrate (ADR-018). Once a
// milestone is closed, never edit an applied file; add a new one.
package migrations

import "embed"

// FS holds the migration files.
//
//go:embed *.sql
var FS embed.FS
