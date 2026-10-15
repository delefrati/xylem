# M9 — Hardening

**Goal:** make Xylem safe to run for a small team: authentication, limits, land throughput, and observability.
**Depends on:** M6 (M7 and M8 recommended).
**Read first:** `07-write-path.md` §Contention mitigation, `13-future.md` §Operations.

## Tasks

- [x] **M9.1 Auth.** Static bearer tokens from a file (`--auth-tokens tokens.toml`: token → identity, roles `read` and `write`). The interceptor enforces roles per RPC. `CreateChangeset` sets the committer from the identity. The client sends `token` from config. Use constant-time comparison.
- [x] **M9.2 Rate limits.** Per-identity token buckets for expensive RPCs (Blame, Log with path, Diff with hunks, MergeBase), configurable, returning `ResourceExhausted` with a retry-after in trailing metadata.
- [ ] **M9.3 Land batcher.** Per (repo, ref) collector per 07, behind a flag `--land-batch-window` (default 0 = off). Benchmark land throughput with 50 concurrent landers, with the batcher on vs off.
- [ ] **M9.4 Object GC (dry-run).** `xylemd gc --dry-run`: mark all objects reachable from changeset trees and derived `value_ref`s, then list unreferenced objects older than 7 days with their total bytes. The sweep is deferred until a human approves an ADR.
- [ ] **M9.5 Tracing.** OpenTelemetry spans across client → daemon → cache → server (OTLP exporter, off by default).
- [ ] **M9.6 Runbook.** `docs/runbook.md`: deploy topology, config, backups (`pg_dump` + bucket versioning), restore drill, alerts on the key metrics, and how to use `verify`.

## Acceptance criteria

- Unauthenticated and wrong-role calls are rejected, with tests for every RPC's role.
- The land batcher improves throughput ≥ 3× at 50 concurrent landers (report numbers), and the concurrency suite still passes with it on.
- GC dry-run on `branchy` after the concurrency suite reports only expected orphans: trees from aborted land attempts and blobs uploaded but never committed.
