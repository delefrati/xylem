# 08 — Git import

`xylemd import` turns a git repository into Xylem changesets. It is resumable and incremental, so it doubles as a **mirror sync** during a migration where git remains the source of truth.

## Command

```
xylemd import --git /path/to/repo.git --branch main [--repo default] [--follow 30s] [--batch 500] [--limit N]
```

- Imports every commit reachable from `refs/heads/<branch>`, parents before children.
- Moves Xylem ref `main` to the imported branch tip at the end of each batch (CAS, `reason = "import"`), maintaining `mainline`.
- `--follow` polls the git repo for new commits and repeats. Combined with `git fetch` in a cron job, this keeps a Xylem mirror current.
- Records progress in `import_state`. Re-running continues where it left off.

## Reading git (`internal/gitsource`)

Shell out to the `git` binary (ADR-010). Git is ground truth and handles every edge case; `git cat-file --batch` is fast.

| Need | Command |
|---|---|
| Commit order | `git rev-list --topo-order --reverse <tip> [^<last_imported>]` (streamed) |
| Commit metadata | `git cat-file --batch` on the commit id → parse the raw commit object (tree, parents, author/committer with tz, message, encoding header) |
| Changes vs first parent | `git diff-tree -r -z --no-commit-id --raw --no-renames <parent> <commit>` (root commit: `--root`). For merges, compare against `<parent1>` explicitly. |
| Blob content | A long-lived `git cat-file --batch` process (request/response over stdin and stdout) |

Wrap each long-lived git process in a small client struct with a mutex. Run 2–4 `cat-file` processes for parallel blob reads.

### Mapping

| git | Xylem |
|---|---|
| mode 100644 / 100755 / 120000 | FILE / EXEC / SYMLINK |
| mode 040000 | (implicit via paths) |
| mode 160000 (submodule) | skipped. Warn once per path. Add `git-extra-submodules` extra listing skipped paths. |
| `diff-tree` status A, M, T | PUT (T = type change, e.g. file ↔ symlink) |
| `diff-tree` status D | DELETE |
| author / committer line | `Name <email>`, unix time, tz offset in minutes |
| commit message | bytes as-is. If an `encoding` header is present, keep the raw bytes and add an `git-encoding` extra |
| commit sha | `git_sha` column plus `git-sha` extra |
| parents | mapped Xylem ids (all must already be imported, guaranteed by topo order) |

Empty commits (no tree change, single parent) are allowed for import only (`allow_empty` flag on the internal create call) so history is complete.

Paths: `diff-tree -z` gives raw bytes. Store them unchanged. A git path containing a `.xylem` component (reserved in Xylem) makes the import **fail** with a clear error listing the commit and path; there is no silent rewrite.

## Pipeline (`internal/importer`)

```
rev-list ──► commit reader ──► change lister ──► blob uploader (N workers) ──► committer (1 goroutine, ordered)
```

1. **Commit reader:** reads the commit object and maps parents (from an in-memory `git sha → Xylem id` map, preloaded from `changesets.git_sha` on resume).
2. **Change lister:** `diff-tree` against the first parent.
3. **Blob uploader:** for each PUT blob sha, check a `git blob sha → (Xylem id, size)` LRU or bloom filter. If unseen, read it via `cat-file`, encode, then `HasObjects`/`Put` (batched). Workers run in parallel; blobs are content-addressed, so order doesn't matter.
4. **Committer:** must create changesets **strictly in topo order**, because a child's core derived data needs its parents. It calls the same internal `CreateChangeset` function the RPC uses (`allow_root`, `allow_empty`, explicit committer).
5. Every `--batch` commits: update `import_state.last_git_sha` and move the `main` ref to the latest imported commit **if it is on the branch's first-parent chain**. Otherwise wait until one is.

Throughput target is ≥ 200 commits/s on a laptop for typical repos. The bottleneck is usually blob reading for the first commits. Pipelining keeps the committer busy.

## Blob mapping table (`migrations/0002_git_blobs.sql`)

```sql
CREATE TABLE git_blobs (
  repo_id   SMALLINT NOT NULL REFERENCES repos(id),
  git_sha   BYTEA    NOT NULL CHECK (octet_length(git_sha) = 20),
  blob_id   BYTEA    NOT NULL CHECK (octet_length(blob_id) = 32),
  size      BIGINT   NOT NULL,
  PRIMARY KEY (repo_id, git_sha)
);
```

The importer inserts rows in batches (COPY, `ON CONFLICT DO NOTHING` via a temp table) after the blobs are stored. On resume it checks this table before reading a blob from git. `verify-import` uses it to compare content without re-reading blobs.

## Verification (`xylemd verify-import`)

For every N-th imported commit (all of them with `--all`):

1. `git ls-tree -r -z -l --full-tree <sha>` → set of `(path, mode, size, git blob sha)`, **filtering out mode 160000** (submodules are skipped by design).
2. In-process recursive tree walk (`treeops` over the mapped changeset's root tree; no RPC needed, the read API arrives in M4) → `(path, mode, size, xylem blob id)`.
3. Compare paths, modes and sizes exactly. For content, map git blob sha to Xylem blob id via the importer's mapping table, or by re-hashing a sample.

Any mismatch fails with a precise report. This is the M3 acceptance gate.

## Edge cases to cover in fixtures

- Root commit; multiple root commits (an unrelated history merged in).
- Octopus merge (3+ parents).
- Merge where the first parent is the *side* branch (as produced by `git pull` on a feature branch).
- File ↔ symlink ↔ directory replacement at the same path across commits.
- Exec-bit-only change.
- Non-UTF-8 path bytes, paths with spaces, newlines and unicode.
- Empty file, large binary file (> 10 MiB).
- Empty commit (`--allow-empty`).
- Commit with an `encoding` header (ISO-8859-1).
- Submodule entry (skipped).
- Identical blob content at many paths (dedup).

## Export (post-MVP)

`xylemd export-git` would write Xylem mainline changes back to a git repo, mapping native changesets to new git commits, so git-only tools keep working. See `13-future.md`.
