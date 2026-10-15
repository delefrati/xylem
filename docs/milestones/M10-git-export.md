# M10 — Git export

**Goal:** write changesets landed natively in Xylem back to a git repository as ordinary git commits, so git-only consumers (GitHub, IDEs, external CI) keep working once Xylem becomes the source of truth. The export is exact, deterministic and resumable, and the importer recognizes exported commits, so a round trip creates no duplicates.
**Depends on:** M6 (M9 recommended: run the export against an authenticated server in any shared setup).
**Read first:** `08-git-import.md` (all, especially §Export), ADR-010, ADR-022, `03-data-model.md` §Changesets.

## Tasks

- [ ] **M10.1 Migration `0003_git_exports.sql`** (per 08 §Export). Add `repo` methods to record and look up mappings in batches (`RecordGitExports`, `GitExports(ids)`, `ExportedBySHA(prefixes)`), with no N+1 lookups. Accept ADR-022, or flag what changed.
- [ ] **M10.2 `internal/gitsink`.** A long-lived `git fast-import --done --quiet` process (one mutex-guarded client, as in `gitsource`). It streams commits with inline blobs, `from`/`merge` parents, `D` before `M`, C-quoted raw path bytes, `author`/`committer` lines built from the changeset (unix seconds, `±hhmm` from `tz_minutes`), and the `encoding` command when a `git-encoding` extra is present. `get-mark` returns commit shas, and `checkpoint` flushes. Also add `UpdateRef(ref, new, old)` via `git update-ref` (CAS) and `HasCommit(sha)`. Unit tests on the `tiny` fixture cover odd paths, negative and non-hour time zones, and messages without a trailing newline.
- [ ] **M10.3 `internal/exporter` pipeline.** Find the **export boundary**: the newest changesets on `main`'s history that are already in git, either imported (`git_sha`, which must exist in the target repo) or in `git_exports`. Use batched lookups, not a full history walk. Export every native changeset after the boundary in topological order, including non-first-parent parents of native merges. Read blobs from the object store, with a blob-sha cache shared through `git_blobs` so the same content isn't streamed twice. Every `--batch`: `checkpoint`, then `RecordGitExports`, then CAS the git branch (crash order in 08). Never call the wall clock: the same changeset always yields the same git sha.
- [ ] **M10.4 Divergence and safety checks.** Before writing, the git branch tip must be a commit Xylem knows (imported or exported) on `main`'s first-parent history. Otherwise fail with `ErrDiverged`, naming both tips, and write nothing to the branch. An imported changeset missing from the target repo is an error (it is never re-created). A missing branch is allowed only when `main`'s history has no imported changesets (native-only repo into an empty git repo).
- [ ] **M10.5 `xylemd export-git` command.** Flags from 08 (`--git`, `--branch`, `--repo`, `--follow`, `--batch`, `--limit`, `--db`, `--store`), a progress log line every 5 s (changesets/s, blobs/s, bytes) and a summary. `--follow` polls `main` and exports new changesets. It does not push: pushing to a remote is the operator's step (documented in 08).
- [ ] **M10.6 Round trip.** The importer preloads `git_exports` into its sha map and maps exported commits to their existing changesets (no new changeset, no ref move). `git:<sha>` revisions resolve exported commits. `Log`/`GetChangesets` fill `git_sha` from `git_exports` for native changesets (batched join), so `xy log`/`xy show` print it. `verify-import` covers exported commits too.
- [ ] **M10.7 `xylemd verify-export`.** For each exported changeset (`--all` or `--sample N --seed S`): compare `git ls-tree -r -z -l --full-tree` with the Xylem tree as `verify-import` does, filtering gitlinks (submodules from imported history stay in git). Also check author, committer, message and the parent mapping exactly, then run `git fsck --strict --no-dangling` on the target.
- [ ] **M10.8 Tests.** A test helper lands scripted native changesets on an imported fixture: non-UTF-8 and quoted paths, file ↔ symlink ↔ directory replacement, exec-bit-only change, empty file, an 11 MiB binary, a native 3-parent merge, a change next to a submodule path, and non-hour time zones. Integration tests (`edge`, `branchy`): export, `verify-export --all`, fsck. **Determinism:** two exports into fresh clones give identical shas. **Kill and resume** (cancel mid-batch, re-run): same shas, no duplicate rows. **Round trip:** re-importing the exported repo into a fresh Xylem repo reproduces every root tree id; re-importing into the same repo is a no-op. **Divergence:** commit directly to the git branch, export fails, branch unchanged. Up-to-date export is a no-op. Native-only repo into an empty git repo.
- [ ] **M10.9 Docs.** Update 02 (component table, layout: `gitsink/`, `exporter/`), 08 §Export with anything learned, 11 (minimum git version if `encoding` raises it), 13 (export done), and add §Migration playbook to 08 (git is the writer → freeze → switch → Xylem is the writer, git mirror kept current by `export-git --follow` plus `git push`).

## Acceptance criteria

- `verify-export --all` and `git fsck --strict` pass after exporting native changesets on top of `edge` and `branchy`.
- Exporting the same history into two fresh clones produces identical commit shas.
- Re-importing an exported repo into a fresh Xylem repo reproduces every root tree id. Into the original repo it is a no-op (0 new changesets).
- A git branch that moved independently is detected, and nothing is written to it.
- A killed export resumes with the same shas and no duplicate mappings.
- Export throughput on `branchy`-sized native history is logged in `progress.md` (target ≥ 200 changesets/s, like import).

## Verification

```bash
make up && make fixtures && make check
X=$(mktemp -d) && git clone --bare testdata/fixtures/edge "$X/edge.git"
bin/xylemd import --git "$X/edge.git" --branch main --repo edge
# land a few native changesets with xy (commit + land) in a workspace of repo "edge", then:
bin/xylemd export-git --git "$X/edge.git" --branch main --repo edge
bin/xylemd verify-export --git "$X/edge.git" --repo edge --all
git -C "$X/edge.git" fsck --strict --no-dangling
bin/xylemd import --git "$X/edge.git" --branch main --repo edge   # round trip: 0 new changesets
```

## Out of scope

Two-way sync (both git and Xylem accepting writes at once) and any reconciliation between them. Exporting drafts or refs other than `main`. Carrying Xylem-only extras (`rebased-from`, …) into git. Pushing to remotes. Editing submodule gitlinks from Xylem. Signed commits.
