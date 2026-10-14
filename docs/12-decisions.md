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

### ADR-020 — Community MinIO images for dev and CI · Proposed
**Context:** The upstream `minio/minio` and `minio/mc` images are no longer pullable (Docker Hub repos removed; quay.io requires auth), so the compose file in 11 can't start. MinIO is only a dev/CI stand-in for S3; the code talks to it through `minio-go`, which works with any S3 endpoint.
**Decision:** Use the community MinIO builds `pgsty/minio` and `pgsty/mc`, pinned to `RELEASE.*` tags. Same binaries, commands, env vars and `mc ready` healthcheck as upstream.
**Consequences:** No code or config changes beyond image names. Alternatives if this fork goes stale: `bitnamilegacy/minio` (frozen at 2025-07), or a different S3 server (SeaweedFS, Garage, RustFS) at the cost of a different init script.

---

<!-- Template:
### ADR-0NN — Title · Proposed
**Context:**
**Decision:**
**Consequences:**
-->
