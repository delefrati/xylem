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
| `make parity-external` | `scripts/fetch-external-fixtures.sh` (cobra and prometheus at pinned shas into `testdata/external/`), then the parity suite against them |
| `make e2e` | `go test -tags e2e ./test/e2e/...` |
| `make fuzz` | Loop over fuzz targets with `-fuzztime=$(FUZZTIME)` (default 30s) |
| `make lint` | `golangci-lint run && buf lint` |
| `make check` | `lint` + `test` + `test-integration` with `-race` + `build` (requires `make up`) |
| `make fixtures` | Generate synthetic fixture repos into `testdata/fixtures/` |
| `make bench-tiers` / `make bench-land` | M8.6 tier benchmark / M9.3 land batcher benchmark (informational; need `make up`) |
| `make bench` | Run Go benchmarks (`-run=^$ -bench=. -benchmem`), integration ones included when services are up |
| `make run-server` | `bin/xylemd server` with dev env vars |
| `make demo` | `up` → build → migrate → import `edge` into repo `demo` → server on :7410 → clone into `/tmp/xylem-demo`, edit, status, diff, commit, land, log, blame → sparse clone of `dup/` |

## Server configuration (flags with env fallbacks)

| Flag | Env | Default | Notes |
|---|---|---|---|
| `--db` | `XYLEM_DB_URL` | `postgres://xylem:xylem@localhost:54329/xylem?sslmode=disable` | |
| `--store` | `XYLEM_STORE` | `fs://./data/objects` | or `s3://xylem-objects?endpoint=localhost:59000&access_key=xylem&secret_key=xylemxylem&secure=false` |
| `--listen` | `XYLEM_LISTEN` | `:7400` | gRPC |
| `--http-listen` | `XYLEM_HTTP_LISTEN` | `:7401` | health, metrics |
| `--object-cache-bytes` | `XYLEM_OBJECT_CACHE_BYTES` | `1073741824` | in-memory LRU |
| `--merge-max-bytes` | `XYLEM_MERGE_MAX_BYTES` | `8388608` | largest file side rebase merges line by line (M7); larger ones conflict as a whole |
| `--land-batch-window` | `XYLEM_LAND_BATCH_WINDOW` | `0` (off) | land batcher (M9.3, 07 §Contention mitigation), e.g. `20ms` |
| `--rate-limits` | `XYLEM_RATE_LIMITS` | `off` | per-caller token buckets (M9.2): `recommended` (`Blame=5:20,Diff=10:40,Log=10:40,MergeBase=20:80`) or a list of `RPC=rate/s:burst` |
| `--log-level` | `XYLEM_LOG_LEVEL` | `info` | |
| `--otlp-endpoint` | `XYLEM_OTLP_ENDPOINT` | empty (off) | OpenTelemetry traces over OTLP/gRPC (M9.5): `host:port` (TLS) or `http://host:port` (plain); also on `cache` and `daemon`. `xy` reads the same env var and traces each command as one span |
| `--auth-tokens` | `XYLEM_AUTH_TOKENS` | empty (auth off) | tokens file, see below (M9.1) |

### Cache tier (`xylemd cache`, M8)

| Flag | Env | Default | Notes |
|---|---|---|---|
| `--upstream` | `XYLEM_UPSTREAM` | (required) | a `xylemd server`; checked at startup via the `xylem-tier` response header |
| `--listen` / `--http-listen` | `XYLEM_LISTEN` / `XYLEM_HTTP_LISTEN` | `:7400` / `:7401` | ObjectService only; `/healthz`, `/readyz` (upstream reachable), `/metrics` |
| `--memory-bytes` | `XYLEM_MEMORY_BYTES` | `4294967296` | memory LRU |
| `--disk-dir` / `--disk-bytes` | `XYLEM_DISK_DIR` / `XYLEM_DISK_BYTES` | off / `107374182400` | disk LRU (fs-store layout) |
| `--max-inflight` | | `256` | concurrent upstream requests; then a queue of 4×, then `ResourceExhausted` |
| `--token` | `XYLEM_TOKEN` | empty | sent upstream on the tier's own calls (cache fills); a client's token is forwarded instead when it sent one. Give it the `read` role only, so writes act as the client |
| `--auth-tokens` | `XYLEM_AUTH_TOKENS` | empty | authenticate this tier's clients, same file format as the server |

### Daemon (`xylemd daemon`, M8)

| Flag | Env | Default | Notes |
|---|---|---|---|
| `--upstream` | `XYLEM_UPSTREAM` | (required) | a `xylemd cache` (a server only with `--allow-direct-upstream`) |
| `--server` | `XYLEM_SERVER` | (required) | the `xylemd server` that RepoService and WriteService calls are proxied to |
| `--socket` | `XYLEM_SOCKET` | `$XDG_RUNTIME_DIR/xylem/daemon.sock` | directory created 0700; a stale socket file is removed |
| `--http-listen` | `XYLEM_HTTP_LISTEN` | `127.0.0.1:7402` | `/healthz`, `/readyz`, `/metrics` |
| `--memory-bytes` | `XYLEM_MEMORY_BYTES` | `268435456` | |
| `--disk-dir` / `--disk-bytes` | `XYLEM_DISK_DIR` / `XYLEM_DISK_BYTES` | `$XDG_CACHE_HOME/xylem/objects` / `21474836480` | shared by every workspace on the machine |
| `--token` / `--auth-tokens` | `XYLEM_TOKEN` / `XYLEM_AUTH_TOKENS` | empty | as for the cache; the socket's 0700 directory already limits clients to the user |

Clients use it with `xy clone unix:///run/user/1000/xylem/daemon.sock <dir> --no-cache` (or `server = "unix://…"` and `cache_max_bytes = 0` in `.xylem/config.toml`).

### Tokens file (M9.1)

```toml
[[token]]
identity = "Ada Lovelace <ada@example.com>"   # committer and ref-log actor
roles = ["read", "write"]                     # write implies read
token = "…"                                   # at least 16 characters, or:
# sha256 = "<64 hex: sha256 of the token>"    # keeps plain tokens out of the file

[[token]]
identity = "Regional Cache <ops@example.com>"
roles = ["read"]
token = "…"
```

Generate tokens with `openssl rand -hex 32`. Tokens travel in plain gRPC metadata: use them only on a private network or behind TLS termination (Xylem has no TLS of its own yet).

`xylemd gc --dry-run [--min-age 168h] [--list] [--json]` (M9.4) reports unreferenced objects older than the grace period, by kind, across every repository (objects are shared). Nothing is deleted until ADR-023 is approved.

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

## Local demo walkthrough

`make demo` (scripts/demo.sh) runs this end to end against the `edge` fixture in a `demo` repository, with its own server on :7410. By hand, with `branchy`:

```bash
make up && make build && bin/xylemd migrate
make fixtures
bin/xylemd import --git testdata/fixtures/branchy --branch main
make run-server &                     # gRPC :7400, HTTP :7401
export XYLEM_AUTHOR="Your Name <you@example.com>"
bin/xy clone localhost:7400 /tmp/ws1 --sparse internal/api,docs
cd /tmp/ws1 && echo hi >> docs/file007.txt
xy status && xy diff                  # with bin/ on PATH
xy commit -m "say hi" && xy land
xy log -n 3 --oneline && xy blame docs/file007.txt
```
