# Progress log

Maintained by the implementing agent. Newest entries first within each section.

## Status

- **Current milestone:** M0 — Bootstrap
- **Last checkpoint approved by human:** — (plan approved: 2026-10-14)
- **Blocked on:** —

## Open questions (for the human)

- [x] Q-001 (M0.1): GitHub owner for the module path (`github.com/<owner>/xylem`)? → answer: `delefrati` (module `github.com/delefrati/xylem`)
- [x] Q-002 (M0.5): License (default MIT)? → answer: MIT

## Deviations from spec

<!-- - YYYY-MM-DD M2.4: <what changed, why, doc updated: 04-derived-data.md> -->
- 2026-10-14 M0.4: **ADR-020 (Proposed)** — upstream `minio/minio`/`minio/mc` images are no longer pullable; switched to pinned `pgsty/minio`/`pgsty/mc`. Needs human approval.
- 2026-10-14 M0.4: `make up` runs `docker compose run --rm minio-init` instead of `compose wait` (which can't see already-exited one-shot containers); doc updated: 11-dev-environment.md
- 2026-10-14 M0.4: `make reset-db` description said the `xy` DB; it's `xylem`; doc updated: 11-dev-environment.md
- 2026-10-14 M0.2: client CLI lives in `cmd/xy` (M0.2, binary name), not `cmd/xylem` as the layout tree said; doc updated: 02-architecture.md
- 2026-10-14 M0.3: Makefile + tools.mk (buf v1.73.0, golangci-lint v2.14.0, grpcurl v1.9.4, protoc-gen-go v1.36.12, protoc-gen-go-grpc v1.6.2); later-milestone targets stubbed; `buf lint` green from M0.6
- 2026-10-14 M0.4: deploy/docker-compose.yml (postgres:17.11, pgsty/minio + pgsty/mc per ADR-020); `make up` verified fresh, rerun and cold

## Milestone results

<!--
### M1 — YYYY-MM-DD
| Acceptance criterion | Evidence | Result |
|---|---|---|
-->

## Measurements

<!-- - YYYY-MM-DD: CreateChangeset 10 files on 1k-file tree: p50 X ms, p99 Y ms (M2.9) -->

## Log

<!-- - YYYY-MM-DD M0.2: skeleton dirs + cobra roots (commit abc1234) -->
- 2026-10-14 M0.1: module `github.com/delefrati/xylem` (go 1.27.2); OWNER replaced in docs
- 2026-10-14 M0.2: internal/* skeleton (19 pkgs), cmd/xy + cmd/xylemd cobra roots with `version`
- 2026-10-14 M0.3: Makefile + tools.mk (buf v1.73.0, golangci-lint v2.14.0, grpcurl v1.9.4, protoc-gen-go v1.36.12, protoc-gen-go-grpc v1.6.2); later-milestone targets stubbed; `buf lint` green from M0.6
- 2026-10-14 M0.4: deploy/docker-compose.yml (postgres:17.11, pgsty/minio + pgsty/mc per ADR-020); `make up` verified fresh, rerun and cold
