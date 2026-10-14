# Progress log

Maintained by the implementing agent. Newest entries first within each section.

## Status

- **Current milestone:** M0 — Bootstrap
- **Last checkpoint approved by human:** — (plan approved: 2026-10-14)
- **Blocked on:** human review of M0 (⛔ end of milestone), incl. ADR-020 (Proposed) and the first CI run after push

## Open questions (for the human)

- [x] Q-001 (M0.1): GitHub owner for the module path (`github.com/<owner>/xylem`)? → answer: `delefrati` (module `github.com/delefrati/xylem`)
- [x] Q-002 (M0.5): License (default MIT)? → answer: MIT

## Deviations from spec

<!-- - YYYY-MM-DD M2.4: <what changed, why, doc updated: 04-derived-data.md> -->
- 2026-10-14 M0.4: **ADR-020 (Proposed)** — upstream `minio/minio`/`minio/mc` images are no longer pullable; switched to pinned `pgsty/minio`/`pgsty/mc`. Needs human approval.
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
| CI is green on the first push | Workflow passes actionlint; MinIO start/bucket steps simulated locally. **Not pushed yet** | ⏳ pending push |

**Summary:** the repo builds, lints, tests, generates protobuf code (committed, freshness-checked), runs Postgres 17.11 and MinIO locally, and has CI. One design change needs approval: **ADR-020** (upstream MinIO images are gone; using pinned `pgsty/minio` and `pgsty/mc`). For the end-of-M0 checkpoint, please confirm the module path (`github.com/delefrati/xylem`), the project name, and the tool choices (versions in `tools.mk`).

<!--
### M1 — YYYY-MM-DD
| Acceptance criterion | Evidence | Result |
|---|---|---|
-->

## Measurements

<!-- - YYYY-MM-DD: CreateChangeset 10 files on 1k-file tree: p50 X ms, p99 Y ms (M2.9) -->

## Log

<!-- - YYYY-MM-DD M0.2: skeleton dirs + cobra roots (commit abc1234) -->
- 2026-10-14 M0.8: cmd/xylemd/main_test.go (version output, link-time and fallback; rejects args), README Getting started
- 2026-10-14 M0.7: .github/workflows/ci.yml — lint, unit, integration (postgres service + MinIO step + mc mb step), build, proto freshness; actionlint clean, MinIO steps simulated locally; not yet pushed
- 2026-10-14 M0.6: buf.yaml v2 (STANDARD/FILE) + buf.gen.yaml (local plugins); 4 protos copied verbatim from 05-api.md; gen/xylem/v1 committed; `make check` green
- 2026-10-14 M0.5: .golangci.yml (v2; 8 linters, gofumpt+goimports, gen/ excluded — verified with a probe), .gitignore, .editorconfig, MIT LICENSE
- 2026-10-14 M0.4: deploy/docker-compose.yml (postgres:17.11, pgsty/minio + pgsty/mc per ADR-020); `make up` verified fresh, rerun and cold
- 2026-10-14 M0.3: Makefile + tools.mk (buf v1.73.0, golangci-lint v2.14.0, grpcurl v1.9.4, protoc-gen-go v1.36.12, protoc-gen-go-grpc v1.6.2); later-milestone targets stubbed; `buf lint` green from M0.6
- 2026-10-14 M0.2: internal/* skeleton (19 pkgs), cmd/xy + cmd/xylemd cobra roots with `version`
- 2026-10-14 M0.1: module `github.com/delefrati/xylem` (go 1.27.2); OWNER replaced in docs
