# 11 — Development environment

Target: Linux (Ubuntu-like) with Docker. macOS should also work.

## Prerequisites

| Tool | Version | Install |
|---|---|---|
| Go | latest stable | https://go.dev/dl |
| Docker + Compose v2 | recent | distro packages or Docker Desktop |
| git | ≥ 2.40 | distro |
| buf, golangci-lint (v2), grpcurl, protoc-gen-go, protoc-gen-go-grpc | pinned | `make tools` (installs into `./bin` via `go install` with versions pinned in `tools.mk`) |

## `deploy/docker-compose.yml`

```yaml
name: xylem
services:
  postgres:
    image: postgres:17.11
    environment:
      POSTGRES_USER: xylem
      POSTGRES_PASSWORD: xylem
      POSTGRES_DB: xylem
    ports: ["54329:5432"]           # non-default port avoids clashing with a local Postgres
    command: ["postgres", "-c", "max_connections=300", "-c", "fsync=off", "-c", "synchronous_commit=off"]  # dev only
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U xylem"]
      interval: 2s
      retries: 30
    volumes: [pgdata:/var/lib/postgresql/data]
  minio:
    image: pgsty/minio:RELEASE.2026-08-04T00-00-00Z   # community MinIO build; upstream images are gone (ADR-020)
    command: server /data --console-address :9001
    healthcheck:
      test: ["CMD", "mc", "ready", "local"]
      interval: 2s
      retries: 30
    environment:
      MINIO_ROOT_USER: xylem
      MINIO_ROOT_PASSWORD: xylemxylem
    ports: ["59000:9000", "59001:9001"]
    volumes: [miniodata:/data]
  minio-init:
    image: pgsty/mc:RELEASE.2026-09-16T00-00-00Z     # ADR-020
    depends_on:
      minio: { condition: service_healthy }
    entrypoint: >
      /bin/sh -c "until mc alias set local http://minio:9000 xylem xylemxylem; do sleep 1; done;
                  mc mb -p local/xylem-objects; exit 0"
volumes: { pgdata: {}, miniodata: {} }
```

`fsync=off` is only for dev and tests. Production config is out of scope for the MVP.

## Makefile targets

| Target | Does |
|---|---|
| `make tools` | Install pinned buf, golangci-lint, grpcurl, protoc-gen-go and protoc-gen-go-grpc into `./bin` |
| `make up` / `make down` | `docker compose -f deploy/docker-compose.yml up -d --wait postgres minio`, then `run --rm minio-init` (blocks until the bucket exists) / `down` |
| `make reset-db` | Drop and recreate the `xylem` DB, then run migrations |
| `make proto` | `buf lint && buf generate` |
| `make build` | `go build -o bin/ ./cmd/...` with `-ldflags "-X main.version=$(git describe --always --dirty)"` |
| `make test` | `go test ./...` (unit only, no Docker) |
| `make test-integration` | `go test -tags integration ./...` |
| `make parity` | Build, generate fixtures, run `go test -tags parity ./internal/parity/...` |
| `make parity-external` | Same against `testdata/external/*` |
| `make e2e` | `go test -tags e2e ./test/e2e/...` |
| `make fuzz` | Loop over fuzz targets with `-fuzztime=$(FUZZTIME)` (default 30s) |
| `make lint` | `golangci-lint run && buf lint` |
| `make check` | `lint` + `test` + `test-integration` with `-race` + `build` (requires `make up`) |
| `make fixtures` | Generate synthetic fixture repos into `testdata/fixtures/` |
| `make bench` | Run Go benchmarks (`-run=^$ -bench=. -benchmem`), integration ones included when services are up |
| `make run-server` | `bin/xylemd server` with dev env vars |
| `make demo` | `up` → migrate → import `edge` fixture → clone it into `/tmp/xylem-demo` |

## Server configuration (flags with env fallbacks)

| Flag | Env | Default | Notes |
|---|---|---|---|
| `--db` | `XYLEM_DB_URL` | `postgres://xylem:xylem@localhost:54329/xylem?sslmode=disable` | |
| `--store` | `XYLEM_STORE` | `fs://./data/objects` | or `s3://xylem-objects?endpoint=localhost:59000&access_key=xylem&secret_key=xylemxylem&secure=false` |
| `--listen` | `XYLEM_LISTEN` | `:7400` | gRPC |
| `--http-listen` | `XYLEM_HTTP_LISTEN` | `:7401` | health, metrics |
| `--object-cache-bytes` | `XYLEM_OBJECT_CACHE_BYTES` | `1073741824` | in-memory LRU |
| `--log-level` | `XYLEM_LOG_LEVEL` | `info` | |
| `--auth-token` | `XYLEM_AUTH_TOKEN` | empty (disabled) | M9 |

`xylemd migrate` applies pending migrations. `xylemd admin init-repo <name>` creates a repo with an empty root changeset and a `main` ref, for repos not imported from git.

## `.golangci.yml` (baseline)

golangci-lint **v2** config format (`version: "2"`):

- `linters.enable`: `govet`, `staticcheck`, `errcheck`, `ineffassign`, `unused`, `gosec` (exclude G304 for CLI file paths), `errorlint`, `revive` (exported-comment rule off).
- `formatters.enable`: `gofumpt`, `goimports` (in v2 these are formatters, not linters).
- Exclude `gen/` from both.

## Repository hygiene

- `.gitignore`: `bin/`, `data/`, `testdata/fixtures/`, `testdata/external/`, `parity-report.json`, `.xylem/`.
- Conventional commits: `feat(m3): import octopus merges`, `fix(derived): …`, `docs: …`, `test: …`.
- Generated code (`gen/`) is committed. CI checks it's up to date (`make proto && git diff --exit-code`).

## Local demo walkthrough (target state after M6)

```bash
make up && make build && bin/xylemd migrate
make fixtures
bin/xylemd import --git testdata/fixtures/branchy --branch main
bin/xylemd server &
bin/xy clone localhost:7400 /tmp/ws1 --sparse dir03,dir07
cd /tmp/ws1 && echo hi >> dir03/file1.txt
bin/xy status && bin/xy diff
bin/xy commit -m "say hi" && bin/xy land
bin/xy log -n 3 && bin/xy blame dir03/file1.txt
```
