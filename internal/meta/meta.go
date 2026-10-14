// Package meta is the Postgres access layer (pgx) and the migrations runner.
//
// The schema is in migrations/ and documented in 03-data-model.md §Postgres
// schema. Packages other than repo (and cmd/) should not depend on meta
// directly; server and importer go through repo.
package meta
