# 12 — Architecture decision records

Format: **Context → Decision → Consequences.** New ADRs are appended with the next number. Status values: Accepted, Superseded by ADR-N, or Proposed (awaiting human approval).

An implementing agent may add **Proposed** ADRs. Only a human changes them to Accepted. Hash-affecting ADRs (004, 006, 015, 017) must never be changed without human approval.

---

### ADR-001 — Go for every component · Accepted
**Context:** One language for server, tiers, client and tools reduces cognitive load for a solo developer. Rust would cut GC overhead in long-lived caches and the FUSE path.
**Decision:** Go everywhere for the MVP.
**Consequences:** Fast iteration and simple deploys. If the daemon or a future VFS hits GC latency limits, rewrite only that component behind the same gRPC interface.

### ADR-002 — Postgres for metadata · Accepted
**Context:** Scion uses Spanner. We need transactions (changeset + files + core derived), CAS refs and indexed path queries on a laptop.
**Decision:** PostgreSQL 17 via pgx. Keep SQL behind `internal/meta` interfaces so a FoundationDB or Spanner backend is possible later.
**Consequences:** Single-writer-node scaling limit (read replicas possible). Fine far beyond MVP scale.

### ADR-003 — Pluggable object store: fs, S3, memory · Accepted
**Decision:** `store.ObjectStore` interface with an fs implementation (default for dev and tests), S3-compatible (MinIO/S3/R2) and in-memory.
**Consequences:** Dev without Docker is possible for unit tests. Production-like runs use MinIO.

### ADR-004 — BLAKE3-256 ids with git-style headers · Accepted · hash-affecting
**Decision:** `id = BLAKE3-256("<kind> <len>\0" ‖ body)`.
**Consequences:** Fast hashing (SIMD) and domain separation between kinds. Xylem ids are not git-compatible; git shas are kept as metadata.

### ADR-005 — zstd frames, same format at rest and on the wire · Accepted
**Decision:** Store `zstd(header ‖ body)` and serve the same bytes.
**Consequences:** Tiers stream bytes from disk or memory without recompressing, as Scion's caches do. Receivers verify the hash after decompressing.

### ADR-006 — Changesets record changes vs first parent; root tree is derived · Accepted · hash-affecting
**Context:** Scion stores changed files per changeset and derives everything else.
**Decision:** The changeset body contains the change list. The root tree is eager derived data and isn't hashed into the changeset id.
**Consequences:** Commit and rebase cost O(change). Tree format can evolve independently. Integrity of the full snapshot relies on deterministic derivation, so `xylemd verify` recomputes and checks it.

### ADR-007 — Commits are created server-side · Accepted
**Decision:** Clients upload blobs and request `CreateChangeset`. There is no local commit or push.
**Consequences:** Requires connectivity, which is accepted as a non-goal. Drafts are visible to every tool immediately.

### ADR-008 — Only `main` and release refs; drafts are ref-less · Accepted
**Decision:** Refs are global state, so keep them minimal. Drafts are addressed by id, and stacks by following parents.
**Consequences:** No branch namespace management. A workspace's `@` plus its stack replaces feature branches.

### ADR-009 — Generation numbers + single skew-binary jump pointer · Accepted
**Decision:** `core_derived` stores gen, fp_depth, fp_parent and fp_jump.
**Consequences:** O(log n) first-parent ancestry and `~N` with one pointer per changeset. General DAG queries use gen-pruned BFS.

### ADR-010 — Import by shelling out to git · Accepted
**Decision:** Use `git rev-list`, `diff-tree` and long-lived `cat-file --batch` processes rather than go-git.
**Consequences:** Exact git semantics and speed. Requires a `git` binary on the importer host.

### ADR-011 — History and blame follow the first parent (mainline) · Accepted
**Decision:** Log, path log and blame use first-parent semantics, matching `git log/blame --first-parent`.
**Consequences:** Simpler, cacheable derived data. Merged-in branch commits appear as their merge changeset. This matches trunk-based development.

### ADR-012 — File-level conflicts in the MVP, diff3 later · Accepted
**Decision:** M6 rebase treats concurrent edits to the same file as a conflict; M7 adds line-level three-way merge.
**Consequences:** The MVP is safe and simple. Hot files will conflict more often until M7.
**Status (M7, done):** rebase and land now merge regular files line by line (`internal/merge.Merge3`, diff3) when both sides changed the content, or both added the file (empty base). Only overlapping or adjacent changes conflict, reported with their region count. Symlinks, binary files and sides over `--merge-max-bytes` (default 8 MiB) still conflict as whole files.

### ADR-013 — Git is the test oracle · Accepted
**Decision:** The parity suite compares Xylem against git for trees, log, diff, blame, merge-base and checkout.
**Consequences:** An objective correctness gate for an AI-driven implementation.

### ADR-014 — Commit generated protobuf code · Accepted
**Decision:** `gen/` is committed. CI verifies it's fresh.
**Consequences:** `go build` works without buf installed.

### ADR-015 — Paths are raw bytes · Accepted · hash-affecting
**Decision:** Paths and names are byte strings (no NUL or `/` in names), stored as BYTEA and proto `bytes`.
**Consequences:** Lossless import of non-UTF-8 git paths. The CLI must print paths carefully (quote invalid UTF-8 like git does).

### ADR-016 — Honor `.gitignore`; no separate ignore format · Accepted
**Decision:** Workspaces use `.gitignore` semantics via go-git's gitignore matcher.
**Consequences:** Imported repos work unchanged.

### ADR-017 — Tree entries include blob size · Accepted · hash-affecting
**Decision:** File entries carry `size`.
**Consequences:** A future VFS can `stat` without fetching blobs, and `ListDirectory` returns sizes for free.

### ADR-018 — Hand-rolled SQL migrations · Accepted
**Decision:** Numbered `.sql` files embedded in the binary and applied by `xylemd migrate`, tracked in `schema_migrations(version, applied_at)`, each in its own transaction.
**Consequences:** No external migration tool dependency.

### ADR-019 — Post-merge testing instead of a locking merge queue · Accepted
**Decision:** Land checks only file-level conflicts. Semantic breakage is caught by post-merge CI and fixed forward or reverted automatically (post-MVP tooling).
**Consequences:** Land throughput scales with the CAS rate, not CI duration.

### ADR-020 — Community MinIO images for dev and CI · Accepted
**Context:** The upstream `minio/minio` and `minio/mc` images are no longer pullable (Docker Hub repos removed; quay.io requires auth), so the compose file in 11 can't start. MinIO is only a dev/CI stand-in for S3; the code talks to it through `minio-go`, which works with any S3 endpoint.
**Decision:** Use the community MinIO builds `pgsty/minio` and `pgsty/mc`, pinned to `RELEASE.*` tags. Same binaries, commands, env vars and `mc ready` healthcheck as upstream.
**Consequences:** No code or config changes beyond image names. Alternatives if this fork goes stale: `bitnamilegacy/minio` (frozen at 2025-07), or a different S3 server (SeaweedFS, Garage, RustFS) at the cost of a different init script.

### ADR-021 — Git-style compaction of the line diff · Accepted
**Context:** Blame parity with git on real repositories was 94.6% (cobra), below the 99% bar. Git and Xylem both produce minimal diffs, but many lines (blank lines, repeated lines) can be matched in several equally minimal ways, and blame attributes them differently.
**Decision:** After the Myers search, `internal/diff` compacts runs of changed lines as git does: each run slides up, then down as far as it can (merging runs it touches), and settles at the lowest position next to a change in the other file, else at the bottom. The first file is compacted against the second, then the second against the first. The result stays minimal. Blame's derived kind moves to version 2 so values computed before are ignored. This is our own implementation of the behavior, not a port of git's code.
**Consequences:** Blame parity: cobra 97.6%, prometheus (18,923 commits) 98.86%; diff hunks match git in more cases (80% vs 64% of random small inputs). The remaining gap comes from parts of git's xdiff that make its diffs non-minimal or differently tie-broken (discarding lines that are unique or too frequent before the search, cost cutoffs on large diffs, the indent heuristic). Matching those means reproducing xdiff (LGPL-2.1) much more closely; the human accepted ≥ 98% blame parity on real repos instead (Q-004); closer xdiff emulation is post-MVP.

### ADR-022 — Git export via `git fast-import`, one writer at a time · Proposed
**Context:** Once Xylem is the source of truth, git-only consumers (GitHub, IDEs, external CI) still need a git copy that keeps up. Native changesets have no git sha, and their bodies are immutable and hashed, so one can't be added later. Running git and Xylem as writers at the same time would need reconciliation between two histories, which is out of scope.
**Decision:** `xylemd export-git` (M10) streams native changesets into the target repo through a long-lived `git fast-import` process (shelling out, as ADR-010 does for reads). Mappings go into a new `git_exports` table, not into changesets. The branch moves only by `git update-ref` CAS. Export is deterministic (no wall clock, committer from the changeset), and it refuses to run when the git branch moved on its own (`ErrDiverged`). The importer treats exported commits as already imported. Xylem-only extras such as `rebased-from` are not exported. Alternatives considered: go-git (a second git implementation to trust), and `hash-object`/`mktree`/`commit-tree` per object (O(tree depth) processes per commit, too slow).
**Consequences:** The export is exact, idempotent across crashes, and round-trips with the importer without duplicates. A migration runs in phases: git as the writer with `import --follow`, then a freeze, then Xylem as the writer with `export-git --follow`. Pushing to remotes stays outside Xylem. The export host needs a `git` binary.

### ADR-023 — Object sweep with a re-mark and a trash prefix · Proposed
**Context:** `xylemd gc --dry-run` (M9.4) finds unreferenced objects: trees and merged blobs from land attempts that never landed, and blobs uploaded but never committed. Deleting them is irreversible, and a sweep races with writes: a client may see an old orphan blob through `HasObjects`, skip the upload and reference it in a new `CreateChangeset` while the sweep deletes it; `CreateChangeset` may find an orphan tree already stored (`Put` skips it) while the sweep removes it.
**Decision (proposed):** `xylemd gc --sweep` (needs a listable store):
1. Record the highest changeset `seq` and derived row count, then mark as the dry run does. Candidates: unreferenced objects older than `--min-age` (default 7 days, at least 1 day).
2. Re-mark from the changesets and derived rows added since step 1 and drop newly referenced candidates.
3. Move the remaining candidates to `trash/<sweep date>/<key>` instead of deleting them (fs rename; S3 copy then delete). `xylemd gc --purge-trash --older-than 30d` deletes trash for real later.
4. Store reads that miss under `objects/` look under `trash/` and restore the object (rare; logged and counted), which covers the race left after step 2. `CreateChangeset`'s blob check keeps failing with `FailedPrecondition` for missing blobs, so clients re-upload.

**Consequences:** No object a committed changeset references can be lost: it is either kept by the re-mark or restored from trash. Space comes back only after the purge. S3 bucket versioning stays the backstop (runbook). Orphan *draft changesets* in Postgres (unbatched land attempts that lost their CAS) keep their trees reachable; collecting drafts is separate work. Until this ADR is accepted, only the dry run exists.

### ADR-024 — OpenTelemetry for tracing · Proposed
**Context:** M9.5 asks for spans across client → daemon → cache → server with an OTLP exporter. Nothing in the approved stack does tracing.
**Decision (proposed):** Add the OpenTelemetry Go API and SDK (`go.opentelemetry.io/otel`, `…/otel/sdk`, v1.47), the OTLP/gRPC trace exporter (`…/exporters/otlp/otlptrace/otlptracegrpc`) and the gRPC instrumentation (`go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc`, v0.72). `internal/tracing` installs a stats handler on every gRPC server and client (W3C trace context in metadata). Tracing is off unless `--otlp-endpoint` (xylemd server, cache, daemon) or `XYLEM_OTLP_ENDPOINT` (xy) is set; while off, the global no-op provider makes the handlers nearly free. A tier's batched upstream fetch continues the trace of the call that opened the batch (`cache.fetch` span); callers that join a batch later are not linked to it. Alternatives: hand-rolled spans in our interceptors with a custom exporter (reinvents OTLP), or no tracing (logs carry request ids already, but not across tiers).
**Consequences:** About a dozen new modules in go.sum (otel was already an indirect dependency through gRPC); gRPC itself is unchanged (v1.84). One trace shows where a slow `xy` command spends its time across tiers. Removing tracing means deleting `internal/tracing` and its call sites.

---

<!-- Template:
### ADR-0NN — Title · Proposed
**Context:**
**Decision:**
**Consequences:**
-->
