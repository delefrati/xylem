# M2 — Metadata and commit engine

**Goal:** changesets can be created (in-process, no RPC yet) with incremental trees, eager core derived data and refs, all stored in Postgres and the object store, with an integrity verifier.
**Depends on:** M1.
**Read first:** `03-data-model.md` §Postgres schema and §Invariants, `04-derived-data.md` §Kind 1 and §Kind 2, `07-write-path.md` §CreateChangeset and §UpdateRef, `10-testing.md` §Database tests.

## Tasks

- [x] **M2.1 Migrations.** Add `migrations/0001_init.sql` (the schema from 03, verbatim unless a bug is found; if so, update 03 too). Add an embedded runner with a `schema_migrations` table, one transaction per file, and an advisory lock so concurrent `migrate` calls are safe. Wire up `xylemd migrate`.
- [ ] **M2.2 Test DB helper.** `testutil.NewDB(t)` using the template-database approach from 10.
- [ ] **M2.3 `internal/meta`.** Define a `Store` interface and a pgx implementation (pool via `pgxpool`):
  - `InsertChangeset(ctx, ChangesetRow, []FileRow, CoreRow) (seq int64, inserted bool, error)` (single tx; COPY for > 1000 file rows)
  - `GetChangesets(ctx, repo, ids)`, `GetCore(ctx, repo, ids)` (batch), `ResolvePrefix(ctx, repo, lo, hi, limit)`, `GetByGitSHA`
  - `GetRef`, `CreateRef`, `CASRef(ctx, repo, name, old, new, mainlineRows, refLogRow)` (single tx)
  - `MainlineLookup(ctx, repo, ref, ids)`, `MainlineAt(ctx, repo, ref, depth)`, `MainlineRewind`
  - `ChangesetFiles(ctx, repo, id)`, `PathHistoryMainline(…)` (query from 04; used in M4, but write and test it here)
  - Repo creation and lookup by name, with an in-process name → id cache.
- [ ] **M2.4 `internal/treeops`.** `Lookup(ctx, objs, root, path) (Entry, found, error)`; `Apply(ctx, objs, root, changes) (newRoot, newTrees []StoredObject, error)` with structural validation (03 rules 4) returning typed errors (`ErrDeleteMissing`, `ErrNoopPut`, `ErrFileDirClash{Path}`). Add an LRU of decoded trees keyed by id. **Property test:** random change sequences; the incremental result must equal a from-scratch build of the flattened file map.
- [ ] **M2.5 `internal/derived` core.** `ComputeCore(ctx, src CoreSource, cs) (CoreRow, error)` (gen, fp_depth, fp_parent, fp_jump per 04; it loads the parents' rows **and** the first parent's `jump` and `jump(jump)` rows, which the jump rule needs), plus a `CoreSource` interface (batch lookup with LRU) and `AncestorAtDepth`, `IsFirstParentAncestor`, `IsAncestor` (gen-pruned BFS), `MergeBases` (paint-down). Property tests against naive implementations on random DAGs.
- [ ] **M2.6 `internal/repo.CreateChangeset`.** Implement steps 1–11 of 07 with options `{AllowRoot, AllowEmpty, Committer *Signature, Now func()}`. Blob existence and size check against the store. Return a domain `Changeset` with core data. Also add `internal/repo/repotest.New(t)` returning a `*repo.Repo` wired with `testutil.NewDB(t)` and a Mem store (a separate package avoids an import cycle with `testutil`).
- [ ] **M2.7 Refs and mainline.** `repo.UpdateRef(ctx, name, expected, new, force, actor, reason)`: fast-forward check via `IsFirstParentAncestor`, mainline rows for the new fp segment (walk `fp_parent` from new down to old), and rewind for force. Add `xylemd admin init-repo <name>` (empty root changeset with `AllowRoot` and `AllowEmpty`; create `main`) and `xylemd admin set-ref`.
- [ ] **M2.8 `xylemd verify`.** For all changesets (or `--sample N`): re-derive `root_tree` from the parent and `changeset_files` and compare; recompute core fields; check `seq(child) > seq(parent)`; check mainline equals the walk of `fp_parent` from each ref tip; check every referenced blob and tree exists (`--objects`). Print a summary and exit non-zero on any violation.
- [ ] **M2.9 Stress test** (integration). Generate 10,000 changesets on a 1,000-file synthetic tree: random puts and deletes, about 5% two-parent merges, `main` advanced in fast-forward steps. Then run `verify --all` in the test. Log the timing to `progress.md`.

## Acceptance criteria

- All unit and integration tests pass under `-race`.
- The stress test passes, and `verify` reports zero violations.
- `CreateChangeset` for a 10-file change on a 1,000-file tree: p50 < 30 ms locally (report the number).
- Concurrent `CreateChangeset` of the same changeset from 10 goroutines yields one row, nine `already_existed`, and no error.

## Verification

```bash
make up && make check
go test -tags integration -run TestStress ./internal/repo/... -v
```

## Out of scope

gRPC, git, lazy derived kinds (blame), path log queries via the API.
