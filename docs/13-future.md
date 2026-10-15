# 13 — Future work (post-MVP)

These are planned and kept consistent with the MVP design. They are not to be built until scheduled in the roadmap.

## Virtual filesystem (FUSE)

- `xy mount <dir> --rev <rev>` uses `github.com/hanwen/go-fuse/v2`.
- Directory listings come from tree objects. `stat` uses tree-entry sizes (ADR-017), and the mtime is the changeset time.
- Blobs are fetched on first `open`/`read`. Writes go to an overlay (copy-on-write directory). `xy status` reads the overlay instead of walking files, so status is O(changed files).
- Prefetch hints: when a directory is listed, prefetch its subtrees in the background.
- Risk: Go GC latency inside FUSE callbacks. Mitigate with off-heap caches; worst case, rewrite the mount daemon in Rust behind the same API (ADR-001).

## Incremental status

- fsnotify-based watcher inside the daemon, per workspace: a journal of changed paths since the last status. Fall back to a full walk on overflow.
- Optional watchman integration.

## Land batcher

See `07-write-path.md` §Contention mitigation. Needed when one ref sees more than ~50 lands/s.

## Line-level merge everywhere

After M7's diff3 in rebase: a `xy resolve` workflow for conflicts. The server returns conflict markers or a three-way view; the client writes them into the workspace and a follow-up commit resolves them.

## Code review (an "Antique"-like tool)

- A review is a draft stack plus comments plus approvals, stored in Postgres, with a web UI.
- Approval rules expressed as policies on paths and change kinds (e.g., comment-only changes need fewer approvals).
- Land requires an approved review (an interceptor check in `Land`).

## Access control

- Repo-level read and write permissions, then **directory-level ACLs**. Checked in `GetObjects`, which requires knowing the path of an object; trees are fetched top-down, so the server can track the authorized subtree set per session or use signed tree-path tokens.
- Audit log of reads of sensitive paths.

## CI integration and automation

- Webhooks / event stream on `main` movement (NOTIFY/LISTEN → gRPC streaming `WatchRef`).
- An automated bisect service using `Log` plus `IsAncestor` (no checkouts needed).
- Automated revert: create a revert changeset from `Diff(first parent, X)` reversed, then land.

## Git export mirror

Scheduled as **M10** (`milestones/M10-git-export.md`, spec in `08-git-import.md` §Export, ADR-022). `xylemd export-git` creates git commits for native changesets on mainline, so git-only consumers (IDEs, external CI) keep working during migration. Still future after M10: two-way sync, exporting refs other than `main`, and pushing to remotes from Xylem.

## Derived data additions

- Copy and rename detection (similarity index over the changes of a changeset), used by `log --follow` and blame.
- A path search index (trigram over paths) for `xy find`.
- Per-tree "last modified" changeset (a fast `ls -l` with history columns).

## Storage and scale

- FoundationDB or Spanner backend for `internal/meta`.
- Object store GC: mark from all changesets plus derived refs, then sweep orphans older than 7 days.
- Large file chunking (content-defined chunking) for blobs over 64 MiB.
- Multi-repo hosting is already allowed by the schema (`repo_id`); add repo management RPCs.
- Daemon seeds (snapshot tarballs) for fast cold starts on ephemeral CI nodes.

## Operations

- Agent "superfans": a Claude agent per component watching metrics and logs, as in the Scion article. Expose a read-only metrics and logs API and runbooks so an agent can triage.
- Rate limiting per client identity. Quotas for expensive queries (blame on huge files).
- OpenTelemetry tracing across tiers.

## Closer git diff emulation (from Q-004)

Blame parity on real repositories is ~98–99% (ADR-021). To approach 100%, `internal/diff` would need more of git's xdiff behavior: discarding unique and very frequent lines before the search, cost cutoffs on expensive diffs, and the indent heuristic. Write it from the behavior, using git as the oracle; xdiff is LGPL-2.1.
