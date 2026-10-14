# M0 — Bootstrap

**Goal:** an empty but fully wired repository: builds, lints, tests, generates protobuf code, runs Postgres and MinIO locally, and has CI.
**Depends on:** nothing.
**Read first:** `02-architecture.md` (code layout, tech stack), `05-api.md`, `11-dev-environment.md`, `12-decisions.md` (ADR-014, ADR-018).

## Tasks

- [x] **M0.1 Module path and name.** If the human hasn't given a GitHub owner, ask once. Replace `OWNER` in every doc and in `go_package` options. Create `go.mod` (`module github.com/<owner>/xylem`, latest stable Go).
- [x] **M0.2 Skeleton.** Create the directory layout from `02-architecture.md` with a `doc.go` (package comment) in each `internal/*` package. Add `cmd/xy` and `cmd/xylemd` with cobra roots and a `version` subcommand that prints version, commit and Go version (ldflags).
- [x] **M0.3 Tooling.** Add `Makefile` and `tools.mk` with pinned versions of buf, golangci-lint, grpcurl, `protoc-gen-go` and `protoc-gen-go-grpc` installed into `./bin` (`make proto` puts `./bin` first on `PATH` so buf finds the plugins). Implement the targets listed in `11-dev-environment.md`. Targets for later milestones can be stubs that print "not yet implemented (Mx)" and exit 0.
- [x] **M0.4 Local services.** Add `deploy/docker-compose.yml` exactly as specified. `make up` must block until both services are healthy and the bucket exists (`docker compose up -d --wait`, then wait for `minio-init` to exit 0). Pin image tags.
- [x] **M0.5 Lint and hygiene.** Add `.golangci.yml` (baseline in 11), `.gitignore`, `.editorconfig`, and a `LICENSE` placeholder (ask the human which license; default MIT if unanswered).
- [x] **M0.6 Protobuf.** Add `buf.yaml` **v2** with `modules: [{path: proto}]` so imports like `xylem/v1/common.proto` resolve (lint `STANDARD`, breaking `FILE`) and `buf.gen.yaml` (protoc-gen-go plus protoc-gen-go-grpc, `paths=source_relative`, out `gen`). Copy the four proto files from `05-api.md` into `proto/xylem/v1/`. `make proto` generates `gen/xylem/v1/*.pb.go`. Commit the generated code.
- [ ] **M0.7 CI.** Add `.github/workflows/ci.yml` with jobs lint, unit, integration (postgres:17 as a service container; MinIO started in a `docker run -d … minio/minio server /data` **step** because service containers can't pass a command, followed by an `mc mb` step; integration tests may be empty for now), build, and a proto-freshness check.
- [ ] **M0.8 Smoke test.** Add `cmd/xylemd/main_test.go` that runs the `version` command, and a README "Getting started" section with the exact commands.

## Acceptance criteria

- `make tools && make up && make proto && make check` passes on a clean clone.
- `bin/xy version` and `bin/xylemd version` print the version info.
- `buf lint` passes on the proto files.
- CI is green on the first push.

## Verification

```bash
make tools && make up && make proto && make check
git status --porcelain   # must be empty after make proto (generated code committed)
```

## Out of scope

Any real logic. ⛔ **Stop for human review at the end of M0.**
