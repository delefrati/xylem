# 06 — Client (`xy` CLI)

The client is deliberately thin. It lays out files, tracks what changed locally, and gives an ergonomic front-end to server RPCs. It never computes history itself.

## Workspace layout

```
<workspace>/
├── <working files…>
└── .xylem/
    ├── config.toml        # server address, repo, cache limits
    ├── state.json         # parent changeset, root tree, sparse profile (atomic replace)
    ├── dirstate           # tracked file metadata (binary, atomic replace)
    ├── objects/ab/cd/<id> # local object cache (stored zstd frames), bounded LRU
    └── lock               # flock() held by mutating commands
```

`config.toml`:

```toml
server = "localhost:7400"          # or unix:///run/user/1000/xylem/daemon.sock (M8)
repo   = "default"
cache_max_bytes = 4294967296       # 4 GiB local object cache; 0 = none (behind a daemon)
token  = ""                        # bearer token (M9.1); XYLEM_TOKEN overrides
```

`state.json`:

```json
{
  "version": 1,
  "parent": "<64 hex>",            // working copy is based on this changeset
  "root_tree": "<64 hex>",
  "sparse": ["services/api", "libs/core"],   // empty list = full checkout
  "added": ["new/file.go"]         // explicitly added untracked paths pending commit
}
```

`dirstate` records, per tracked path: `mode`, `size`, `mtime_ns`, `blob_id`. It is sorted by path and gob-encoded, with a header `XYLDS1` and a written-at timestamp. It is rewritten via a temp file and rename. If it's missing or has a bad version, the client rebuilds it by hashing files.

## Commands (MVP)

| Command | Behaviour |
|---|---|
| `xy clone <server> <dir> [--repo R] [--rev main] [--sparse a,b] [--no-cache]` | Create the workspace, resolve the revision, fetch trees (scoped), fetch blobs, write files and state. |
| `xy status` | List `M`/`A`/`D`/`?` paths (sparse scope only) with paths relative to the cwd. `--json` for tools. |
| `xy diff [paths]` | Working copy vs `@`, unified diff. Computed locally from the cached base blobs and working files, using the same `internal/diff`. |
| `xy add <paths>` | Mark untracked files for inclusion in the next commit. |
| `xy commit -m <msg>` | Upload new blobs, call `CreateChangeset(parent=@)`, move `@` to the new draft, update dirstate. Prints the short id. |
| `xy log [path] [-r rev] [-n N]` | Server `Log`. Defaults to `@`. |
| `xy show [rev]` | Changeset metadata plus diff vs its first parent (server `Diff`). |
| `xy blame <path> [-r rev]` | Server `Blame`. Format: `<short id> (<author> <date> <line no>) <content>`, as in `git blame`. |
| `xy cat <path> [-r rev]` | Server `ReadFile`. Works without a workspace when given `--server` and `--repo`. |
| `xy ls [path] [-r rev] [-R]` | Server `ListDirectory`. |
| `xy checkout <rev>` | Move the working copy to rev. Refuses if dirty files would be overwritten (`--force` discards). |
| `xy stack` | Show draft changesets between `@` and mainline (fp chain walk via `GetChangesets`). |
| `xy sync` | Fetch the `main` tip. With no drafts, `checkout main`. With drafts, call `Rebase(stack onto main)` and check out the rebased top. Refuses when dirty. |
| `xy land` | `Land(stack, max_attempts=100)`, then check out the new tip. On conflict, print the conflict list and leave the workspace unchanged. Refuses with local changes (exit 4). |
| `xy sparse list\|add <dir>\|remove <dir>` | Change the profile, then fetch or remove files accordingly. |
| `xy cache stats\|gc` | Local object cache size and LRU eviction. |
| `xy version` | Build info. |

Global flags: `-C <dir>`, `--server`, `--repo`, `--json` (where it makes sense), `-v`.

Exit codes: `0` ok, `1` error, `2` usage, `3` conflicts (land, sync), `4` dirty workspace refused.

## Algorithms

### Fetch trees and blobs (`internal/client/fetch.go`)

The full protocol is in `09-fetch-and-caching.md`. Client side:

1. `want = [root_tree]`. In a loop: drop ids already in the local cache, `GetObjects` in batches of ≤ 1024 with up to 8 concurrent streams, then decode the trees.
2. For each tree entry of mode `TREE` whose path is **in sparse scope**, add it to the next `want` level. A path is in scope if it is an ancestor of a sparse dir (needed to reach it), or equal to or under a sparse dir. All comparisons are **by whole path components** (`serv` is not an ancestor of `services`). The root's files are always in scope.
3. Collect the blob ids of in-scope files and fetch the missing ones the same way.
4. Verify the hash of every received object before caching it.

### Checkout (`internal/client/checkout.go`)

Given the current (`@` tree, dirstate) and a target tree:

1. Compute a scoped tree diff between the current and target root trees, skipping equal subtree ids.
2. **Dirty check:** for each path in the diff, if the working file differs from the dirstate (see status), abort with exit 4 unless `--force`.
3. Fetch the needed trees and blobs.
4. Apply: delete removed files (then remove directories left empty), and write added or changed files via a temp file in the same directory, `fsync`, rename. Set permissions to `0644`/`0755`, and create symlinks with `os.Symlink`.
5. Update dirstate entries with the fresh `lstat`. Write `state.json` last, so a crash before that leaves the old state, and `xy status` shows the partial work as modifications.

### Status (`internal/client/status.go`)

1. Walk the in-scope directories, skipping `.xylem/` and anything matched by `.gitignore` files (git semantics, including nested `.gitignore`).
2. For each path:
   - In the dirstate with the same `size`, `mtime_ns` and mode → **clean**. Exception (racy-git): if `mtime_ns ≥ dirstate.written_at`, hash it anyway.
   - Otherwise hash the content (BLAKE3 of the blob header plus content) and compare to `blob_id`.
   - Tracked but missing on disk → `D`. Not tracked and not ignored → `?`, or `A` if listed in `state.added`.
3. Refresh the dirstate entries whose content matched but whose stat changed, so the next status is fast.
4. Hashing runs in parallel (GOMAXPROCS workers).

Future: fsnotify- or watchman-based incremental status (see `13-future.md`).

### Commit (`internal/client/commit.go`)

1. Run status to get changes: modified, deleted, and added (`state.added`). `?` files are not included unless `-A`.
2. Build blobs for new content. `HasObjects` → `PutObjects` for missing ones (packed up to ~1 MiB per message; a larger blob goes alone).
3. `CreateChangeset(parents=[@], author from config or git config fallback, message, changes)`.
4. Set `@` to the returned id and `root_tree` from the response. Update dirstate entries for the committed paths and clear `state.added`.

There is no local commit and nothing to push. A draft exists on the server the moment `commit` returns.

### Stack discovery

The stack is the fp chain from `@` down to (excluding) the first changeset with `on_mainline = true`. Fetch it via `GetChangesets` in batches, following `parents[0]`. Cap at 1,000 entries.

## Token

Sent as `authorization: Bearer <token>` on every call. Look up in order: `XYLEM_TOKEN` env, `token` in `.xylem/config.toml`, then a top-level `token` in `~/.config/xylem/config.toml`. Empty means none (a server without `--auth-tokens`). `xy clone` uses the env or user config and does not save the token in the new workspace. `config.toml` is written 0600.

## Author identity

Look up in order: `XYLEM_AUTHOR` env (`"Name <email>"`), `[user] name/email` in `~/.config/xylem/config.toml`, then `git config user.name/user.email`. Error if none is found.

## Output style

- Short ids are 12 hex characters by default.
- Human output goes to stdout, logs and progress to stderr. Progress bars only when stderr is a TTY.
- `--json` output is one JSON document (or JSON lines for streams), stable enough for scripts.
