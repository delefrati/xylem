# M5 — Client read path

**Goal:** `xy` can clone (full and sparse), check out other revisions, show status and local diff, and run every history query, with checkouts byte-identical to git.
**Depends on:** M4.
**Read first:** `06-client.md` (all), `09-fetch-and-caching.md` §Fetch protocol, ADR-016.

## Tasks

- [x] **M5.1 CLI skeleton.** Cobra command tree for every MVP command in 06 (unimplemented ones print "not yet implemented (M6)"). Add global flags, exit codes, `--json` plumbing, and a gRPC connection factory (`host:port`, with `unix://` reserved for M8).
- [x] **M5.2 Workspace.** `client.Workspace` with `Init(dir, cfg)` and `Open(dir)` (walks up from cwd to find `.xylem/`). Atomic `state.json` writes and `flock` on `.xylem/lock` for mutating commands. Load and save `config.toml`.
- [x] **M5.3 Local object cache.** Sharded files under `.xylem/objects`, verify on insert, size accounting, LRU eviction by atime, and `xy cache stats|gc`.
- [x] **M5.4 Fetcher.** Scoped breadth-first tree fetch, then blob fetch, with batching (≤ 1024), 8 concurrent streams, and progress reporting to stderr on a TTY. Sparse scope helper (`InScope(path)`) with tests for prefix and under semantics.
- [x] **M5.5 Dirstate.** Format per 06 (`XYLDS1` header, gob, written-at timestamp), atomic save, and rebuild-by-hashing when missing.
- [ ] **M5.6 Checkout engine.** Diff the current and target trees (scoped), dirty check, fetch, apply (atomic writes, modes, symlinks, empty-dir cleanup), update dirstate, write state last. Handle type changes (file ↔ symlink ↔ dir) by removing before writing.
- [ ] **M5.7 `xy clone`.** Full and `--sparse`. Fails if the target dir exists and isn't empty.
- [ ] **M5.8 Status and local diff.** Walk with `.gitignore` (go-git matcher, nested files), racy-mtime rule, parallel hashing, `A`/`M`/`D`/`?`, and paths relative to the cwd. `xy diff` uses `internal/diff` on the base blob (from the cache, fetched if missing) vs the working file.
- [ ] **M5.9 Query commands.** `log`, `show`, `blame`, `cat`, `ls`, all thin wrappers over RPCs with human and `--json` output. `@` resolves client-side.
- [ ] **M5.10 `xy checkout <rev>` and `sparse add/remove/list`.** Sparse changes fetch newly in-scope paths or delete out-of-scope files (refuse if any are dirty).
- [ ] **M5.11 Clone parity + e2e.** Add the Clone row of the parity suite (full and 3 random sparse profiles per fixture, compared to `git archive` extraction, including exec bits and symlink targets). Add an e2e test package `test/e2e` running real binaries against an in-process server: clone, modify, status, diff, checkout another rev, and sparse add/remove.

## Acceptance criteria

- Clone parity at 100% on `tiny`, `edge` and `branchy` (full and sparse).
- A sparse clone fetches **only** trees on the path to and under the profile, plus their blobs (asserted via a server-side object counter in the test).
- `xy status` on an unchanged `branchy` checkout runs in < 200 ms after the first run (stat-only path).
- A crash simulation (kill between file writes and state write) leaves a workspace where `status` reports the partial changes and `checkout --force` recovers.

## Verification

```bash
make up && make check && make parity && make e2e
```

## Out of scope

Commit, land and sync (M6), the daemon socket (M8), fsnotify.
