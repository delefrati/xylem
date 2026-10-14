# M4 — Read API and git parity

**Goal:** a running `xylemd server` answering every read RPC, proven equal to git by the parity suite.
**Depends on:** M3.
**Read first:** `05-api.md`, `04-derived-data.md` (§Kind 3, 4, 5, plus ancestry), `03-data-model.md` §Revision syntax, `10-testing.md` §Parity suite.

## Tasks

- [x] **M4.1 Server skeleton.** `xylemd server`: gRPC server on `--listen`, HTTP on `--http-listen` (`/healthz`, `/readyz`, `/metrics`), graceful shutdown on SIGTERM (drain for 10 s), and reflection enabled. Interceptors: request id, slog access log, Prometheus metrics, panic recovery, and an auth hook (no-op). Repo name → id resolution with a cache.
- [x] **M4.2 `internal/revision`.** Parser for the syntax table in 03 (`main~3^2`, `git:abc123`, hex prefixes) and a resolver against `repo`. Ambiguity errors list candidates. Unit tests for the parser; integration tests for the resolver.
- [x] **M4.3 ObjectService Get/Has.** Parallel store reads (worker pool), an in-memory LRU of stored frames (`--object-cache-bytes`), streaming chunked at ~4 MiB, and `missing` reported in the final message. Return `Unimplemented` for `PutObjects` until M6.
- [x] **M4.4 Changeset and ref RPCs.** `ResolveRevision`, `GetChangesets` (with `include_changes`), `ListRefs`, `UpdateRef` (admin semantics via `repo.UpdateRef`). Add proto ↔ domain converters in `internal/server/convert.go` with round-trip tests.
- [x] **M4.5 Tree RPCs.** `ListDirectory` (non-recursive and recursive with pagination; recursive walks trees depth-first in path order), and `ReadFile` (streams 1 MiB chunks; errors on directories).
- [x] **M4.6 `internal/diff`.** Myers O(ND) line diff with linear-space refinement (or plain O(ND) with a size cap of 50k lines per side, falling back to "binary-like" whole-file replacement above it; document the choice). Add unified hunk generation with N context lines and binary detection (NUL in the first 8 KiB). Property tests from 10.
- [x] **M4.7 Tree diff and Diff RPC.** `treeops.Diff(a, b, prefixes)` skipping equal subtrees, plus the fast path from `changeset_files` when `from` is the first parent. Statuses per 05. Hunks optional.
- [ ] **M4.8 Log RPC.** Repo log (fp chain walk with pagination by fp_depth) and path log (draft segment + mainline query + fallback, per 04). The page token encodes `(seq cursor, fp_depth cursor)`.
- [ ] **M4.9 Ancestry RPCs.** `IsAncestor`, `MergeBase` wired to the M2 functions, with a cost limit (visited-node cap → `ResourceExhausted`).
- [ ] **M4.10 Derived engine + blame.** `derived.Engine` (inline vs `value_ref` storage, singleflight, LRU), the `blame` kind (encoding and iterative algorithm from 04), and the `Blame` RPC (resolve the last modifier, load blame, join lines with content, attach origin metadata).
- [ ] **M4.11 Parity suite.** `internal/parity` implementing every check row from 10 *except* Clone (M5), plus `xylemd parity --git <dir> --server <addr> [--seed] [--checks …]` writing `parity-report.json`. `make parity` = fixtures → import each → start an in-process server → run all checks. Add parity tests under `-tags parity`.
- [ ] **M4.12 grpcurl docs.** Add a `docs/api-examples.md` with working `grpcurl` invocations for each RPC against the demo repo.

## Acceptance criteria

- Parity: 100% on `tiny` and `edge` for all checks; on `branchy`, 100% for tree, log, path log, diff names, merge-base, is-ancestor and read file, and ≥ 99% blame lines.
- Blame warm latency < 50 ms for a `branchy` file (report cold and warm).
- `buf breaking` passes against the M0 protos, or ADRs are added for intentional changes made before any external consumer exists.

## Verification

```bash
make up && make check && make parity
bin/xylemd parity --git testdata/fixtures/branchy --server localhost:7400   # after manual import + server
```

## Out of scope

Writes, the client CLI and cache tiers.
