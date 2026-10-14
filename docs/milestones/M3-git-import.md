# M3 — Git import

**Goal:** import any reasonable git repository into Xylem (resumable, incremental), proven tree-for-tree identical to git.
**Depends on:** M2.
**Read first:** `08-git-import.md` (all), `10-testing.md` §Fixture repositories, ADR-010.

## Tasks

- [x] **M3.1 Fixture generators.** `scripts/make-fixture-repo.sh <dir> tiny|edge` (deterministic dates and identities; every edge case listed in 08), and `scripts/genrepo` (Go program) for `branchy` (~2,000 commits, ~300 files, random feature branches with merges, fixed seed). Add `make fixtures` to generate all three under `testdata/fixtures/`.
- [x] **M3.2 `internal/gitsource`.** `RevList(ctx, tip, exclude) iter`; `CatFile` pool (long-lived `git cat-file --batch`, request/response framing, N processes); `ReadCommit(sha)` parsing the raw commit (tree, parents, author/committer with tz, encoding, message bytes); `DiffTree(parent, commit)` parsing `-z --raw` output (statuses A/M/D/T, modes, shas, raw path bytes). Unit tests using the `tiny` and `edge` fixtures (they run `git`, so mark them to skip if `git` is missing).
- [x] **M3.3 Mapping layer.** Convert git commit and changes into a `changeset.Changeset` (modes, signatures, extras `git-sha`, `git-encoding`, `git-extra-submodules`), with a warning callback for skipped submodules.
- [ ] **M3.4 Pipeline.** `internal/importer` with the reader → lister → blob uploaders → ordered committer stages from 08. Use a `git sha → Xylem id` map (preloaded on resume), a blob-sha cache (LRU), `HasObjects`-style batching against the store, and `repo.CreateChangeset` with `AllowRoot`/`AllowEmpty`/explicit committer.
- [ ] **M3.5 Progress and resume.** Every `--batch` commits, update `import_state` and advance `main` via `repo.UpdateRef(reason="import")` when the last imported commit is on the branch's first-parent chain. A killed import must resume without duplicates (idempotent create plus a preloaded map).
- [ ] **M3.6 `xylemd import` command.** Flags from 08, plus a progress log line every 5 s (commits/s, blobs/s, bytes). Add `--follow` polling mode.
- [ ] **M3.7 `xylemd verify-import`.** Tree comparison per 08, using an in-process tree walk via `repo`/`treeops`, not RPCs (`--all` or `--sample N --seed S`). Map git blob shas to Xylem ids via the `git_blobs` table (migration `0002_git_blobs.sql`, defined in 08), which the importer fills as it uploads blobs. The table also serves as the persistent blob-sha cache on resume.
- [ ] **M3.8 Tests.** Integration tests importing `tiny`, `edge` and `branchy`, then `verify-import --all`. Kill-and-resume test (cancel the context mid-import, re-run, verify). Incremental test (add commits to the fixture, re-import, verify the new commits only).

## Acceptance criteria

- `verify-import --all` passes for `tiny`, `edge` and `branchy`.
- Re-running import on an up-to-date repo is a no-op (0 new changesets) and finishes in < 2 s.
- `main` points at the mapped tip of the git branch, and `xylemd verify --all` passes.
- Import throughput is logged in `progress.md` for `branchy` (and for the external small repo if fetched).

## Verification

```bash
make up && make fixtures && make check
bin/xylemd import --git testdata/fixtures/edge --branch main && bin/xylemd verify-import --git testdata/fixtures/edge --all
```

## Out of scope

Export to git and rename detection.
