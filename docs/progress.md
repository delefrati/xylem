# Progress log

Maintained by the implementing agent. Newest entries first within each section.

## Status

- **Current milestone:** M1 — Core objects and stores
- **Last checkpoint approved by human:** M0 — 2026-10-14 (module path, name, tools confirmed; ADR-020 accepted)
- **Blocked on:** —

## Open questions (for the human)

- [x] Q-001 (M0.1): GitHub owner for the module path (`github.com/<owner>/xylem`)? → answer: `delefrati` (module `github.com/delefrati/xylem`)
- [x] Q-002 (M0.5): License (default MIT)? → answer: MIT

## Deviations from spec

<!-- - YYYY-MM-DD M2.4: <what changed, why, doc updated: 04-derived-data.md> -->
- 2026-10-14 M1.5: `ValidateShape` also checks `parent_count ≤ 16` (shape part of rule 1) and that extra keys are non-empty UTF-8 (proto `map<string, bytes>` cannot carry invalid UTF-8 keys); `Summary()` also drops a trailing `\r` and replaces NUL with U+FFFD (Postgres TEXT cannot store NUL). Not hash-affecting.
- 2026-10-14 M1.5: new package `internal/canon` (shared canonical varint/bytes/id reader used by trees and changesets) instead of duplicating it; doc updated: 02-architecture.md
- 2026-10-14 M1.4: 10-testing.md object property "any single-byte mutation of a stored frame fails Verify" is unachievable with standard zstd (e.g. raising the window descriptor decodes to identical bytes). Reworded to "no mutation verifies with different content", which is the integrity guarantee; doc updated: 10-testing.md
- 2026-10-14 M0.4: **ADR-020 (Accepted 2026-10-14)** — upstream `minio/minio`/`minio/mc` images are no longer pullable; switched to pinned `pgsty/minio`/`pgsty/mc`.
- 2026-10-14 M0.4: `make up` runs `docker compose run --rm minio-init` instead of `compose wait` (which can't see already-exited one-shot containers); doc updated: 11-dev-environment.md
- 2026-10-14 M0.4: `make reset-db` description said the `xy` DB; it's `xylem`; doc updated: 11-dev-environment.md
- 2026-10-14 M0.2: client CLI lives in `cmd/xy` (M0.2, binary name), not `cmd/xylem` as the layout tree said; doc updated: 02-architecture.md

## Milestone results

### M0 — 2026-10-14

Verified on a fresh `git clone` of 3e549f4: `make tools && make up && make proto && make check` → exit 0 (23 s with warm caches); `git status --porcelain` empty afterwards.

| Acceptance criterion | Evidence | Result |
|---|---|---|
| `make tools && make up && make proto && make check` passes on a clean clone | Clean-clone run above; golangci-lint 0 issues, `go test -race -tags integration ./...` ok, build ok | ✅ |
| `bin/xy version` and `bin/xylemd version` print the version info | `xy 3e549f4 (commit 3e549f4, go1.27.2 linux/amd64)`, same for `xylemd`; covered by `TestVersionCommand` | ✅ |
| `buf lint` passes on the proto files | `buf lint` (STANDARD) clean on the 4 protos copied verbatim from 05-api.md | ✅ |
| CI is green on the first push | GitHub Actions run 38611733577 on bb60187: lint, unit, integration, build, proto freshness all success (~5 min, cold tool cache) | ✅ |

**Summary:** the repo builds, lints, tests, generates protobuf code (committed, freshness-checked), runs Postgres 17.11 and MinIO locally, and has CI. Approved by the human on 2026-10-14, including **ADR-020** (pinned `pgsty/minio` and `pgsty/mc` replace the unavailable upstream MinIO images).

<!--
### M1 — YYYY-MM-DD
| Acceptance criterion | Evidence | Result |
|---|---|---|
-->

## Measurements

<!-- - YYYY-MM-DD: CreateChangeset 10 files on 1k-file tree: p50 X ms, p99 Y ms (M2.9) -->

## Log

<!-- - YYYY-MM-DD M0.2: skeleton dirs + cobra roots (commit abc1234) -->
- 2026-10-14 M1.7: internal/store/storetest contract suite (put/get, not found, has incl. order+dups, idempotent put, 32 concurrent same-id puts, 64 distinct, 24 MiB object (S3 multipart), canceled ctx); passes for mem, fs and s3 (-tags integration, MinIO); mutation-checked; store coverage 88.7% with integration
- 2026-10-14 M1.6: internal/store — ObjectStore, ErrNotFound, Key, Open(mem/fs/s3 URL, secrets redacted in errors); Mem (copying), FS (temp+fsync+rename, skip if present), S3 (minio-go v7.3.0, Has via StatObject x32 via x/sync v0.24.0 errgroup); S3 smoke-checked against local MinIO; full contract suite is M1.7
- 2026-10-14 M1.5: internal/changeset — Changeset/Signature/Change, Encode (sorts copies; rejects dup paths and op/mode mismatches), ID, Decode (canonical only), ValidateShape(ShapeOptions{AllowEmpty}), Summary; internal/canon extracted; 99.4% coverage, hand-computed layout test
- 2026-10-14 M1.4: internal/object tree encoding — Mode, Entry, Tree, EncodeTree (validates order/names/modes, rejects size on TREE), DecodeTree (canonical only: minimal varints, no trailing bytes, bounded prealloc), Lookup; hand-computed layout test; backfilled M1.3 mutation property; 98.7% coverage
- 2026-10-14 M1.3: internal/object framing — Kind, Header, ID, Encode/Decode/DecodeVerify/Verify; shared zstd level-3 encoder/decoder (concurrent-safe, internally pooled), 64 MiB + 1 KiB decode cap (bomb-tested incl. streamed frames), strict canonical header parsing; klauspost/compress v1.20.1; 96.7% coverage
- 2026-10-14 M1.2: internal/paths — ValidName/ValidPath (rule-specific errors), Split/Join/Parent/Base, IsUnder (dir itself counts; root = empty), Compare, DirPrefixRange (strictly below dir, nil = open); 100% coverage; FuzzValidPath 30 s clean
- 2026-10-14 M1.1: internal/hash — ID, Sum, String/Short/IsZero, Compare, ParseHex, ParsePrefix (lo/hi byte range); blake3 v0.2.4, go-cmp v0.7.0; 100% coverage, BLAKE3 official vectors
- 2026-10-14 M0.8: cmd/xylemd/main_test.go (version output, link-time and fallback; rejects args), README Getting started
- 2026-10-14 M0.7: .github/workflows/ci.yml — lint, unit, integration (postgres service + MinIO step + mc mb step), build, proto freshness; actionlint clean, MinIO steps simulated locally; not yet pushed
- 2026-10-14 M0.6: buf.yaml v2 (STANDARD/FILE) + buf.gen.yaml (local plugins); 4 protos copied verbatim from 05-api.md; gen/xylem/v1 committed; `make check` green
- 2026-10-14 M0.5: .golangci.yml (v2; 8 linters, gofumpt+goimports, gen/ excluded — verified with a probe), .gitignore, .editorconfig, MIT LICENSE
- 2026-10-14 M0.4: deploy/docker-compose.yml (postgres:17.11, pgsty/minio + pgsty/mc per ADR-020); `make up` verified fresh, rerun and cold
- 2026-10-14 M0.3: Makefile + tools.mk (buf v1.73.0, golangci-lint v2.14.0, grpcurl v1.9.4, protoc-gen-go v1.36.12, protoc-gen-go-grpc v1.6.2); later-milestone targets stubbed; `buf lint` green from M0.6
- 2026-10-14 M0.2: internal/* skeleton (19 pkgs), cmd/xy + cmd/xylemd cobra roots with `version`
- 2026-10-14 M0.1: module `github.com/delefrati/xylem` (go 1.27.2); OWNER replaced in docs
