# Runbook

How to run Xylem for a small team: what to deploy, how to configure it, how to back it up and restore it, what to alert on, and how to check integrity. Commands assume the binaries from `make build` (`bin/xylemd`, `bin/xy`). Flags and their env vars are listed in [11-dev-environment.md](11-dev-environment.md).

## 1. Topology

```
 developer machines                 region / office                 central
 ┌──────────────────────┐          ┌─────────────────┐       ┌───────────────────────────┐
 │ xy ──unix──▶ xylemd  │──tcp────▶│  xylemd cache   │──────▶│ xylemd server (1..n)      │
 │       daemon         │  objects │  (memory+disk)  │       │   ├──▶ PostgreSQL 17      │
 │   (shared disk cache)│──────────┼─────────────────┼──────▶│   └──▶ S3 bucket (objects)│
 └──────────────────────┘ history, writes (proxied by the daemon)  └───────────────────────────┘
```

| Component | How many | State | Notes |
|---|---|---|---|
| `xylemd server` | 1+ behind a TCP load balancer | none (caches only) | Every server can serve every RPC. Lands from several servers race safely through the ref CAS. |
| PostgreSQL 17 | 1 primary (+ replica) | **the history**: changesets, refs, mainline, derived data | Back up (§4). Each process opens up to max(4, CPUs) connections; set `pool_max_conns=N` in the URL to change it. |
| Object store (S3, MinIO, R2) | 1 bucket | **the content**: blobs, trees, large derived values | Turn on **versioning** (§4). `fs://` is for one-machine setups. |
| `xylemd cache` | 0+ per region | memory + disk LRU | ObjectService only. Its upstream must be a server. |
| `xylemd daemon` | 0–1 per machine | disk LRU shared by all workspaces | Clients use `server = "unix://…"`, `cache_max_bytes = 0`. Its upstream must be a cache (`--allow-direct-upstream` for a server). |

Tiers check their upstream's kind at startup and refuse the wrong one (daemon → cache → server).

Ports: gRPC `:7400`, HTTP `:7401` (`/healthz`, `/readyz`, `/metrics`); daemon HTTP `127.0.0.1:7402`.

**No TLS of its own.** gRPC is plaintext and tokens travel in metadata. Run the server and caches on a private network, or put a TLS-terminating gRPC proxy (Envoy, nginx `grpc_pass`) in front of them.

## 2. Configuration

### Server

```bash
xylemd migrate --db "$XYLEM_DB_URL"            # before starting a new version
xylemd server \
  --db "postgres://xylem:…@db:5432/xylem" \
  --store "s3://xylem-objects?endpoint=s3.example.com&secure=true&region=…" \
  --auth-tokens /etc/xylem/tokens.toml \
  --rate-limits recommended \
  --land-batch-window 20ms \
  --object-cache-bytes 4294967296 \
  --otlp-endpoint http://otel-collector:4317    # optional
```

| Setting | Recommendation |
|---|---|
| `--auth-tokens` | Always set outside a laptop. Without it every call is allowed (the server logs a warning). |
| `--rate-limits` | `recommended` (Blame 5/s burst 20, Log-with-path and Diff-with-hunks 10/s burst 40, MergeBase 20/s burst 80 per caller), then tune from `xylem_rate_limited_total`. |
| `--land-batch-window` | `20ms` once several people or agents land on `main` at the same time (§5.4). It costs up to 20 ms on a lone land. |
| `--object-cache-bytes` | 1–4 GiB. |
| `--merge-max-bytes` | Default 8 MiB. Larger files conflict as a whole instead of merging line by line. |
| S3 credentials | Prefer `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` in the environment over `access_key=` in the URL (which shows up in `ps`). |

### Tokens

`/etc/xylem/tokens.toml`, mode 0600, owned by the service user:

```toml
[[token]]
identity = "Ada Lovelace <ada@example.com>"   # committer of her lands, actor in the ref log
roles = ["read", "write"]
sha256 = "<64 hex>"            # printf %s "$TOKEN" | sha256sum — keeps plain tokens out of the file

[[token]]
identity = "Regional cache <ops@example.com>"
roles = ["read"]
token = "…"                    # plain form also works (≥ 16 characters)
```

- Generate tokens with `openssl rand -hex 32`. Give each person, CI system and tier its own token.
- Give caches and daemons `read` only. A client's own token is forwarded for writes, so writes are attributed to the client.
- The file is read at startup. **To add, rotate or revoke a token, edit the file and restart the servers** (a rolling restart is fine: clients retry).
- Clients: `XYLEM_TOKEN`, `token` in `.xylem/config.toml`, or `token` in `~/.config/xylem/config.toml`.

### Cache and daemon

```bash
XYLEM_TOKEN=<read token> xylemd cache --upstream server.internal:7400 \
  --auth-tokens /etc/xylem/tokens.toml --disk-dir /var/cache/xylem --disk-bytes 107374182400
XYLEM_TOKEN=<read token> xylemd daemon --upstream cache.office:7400 --server server.internal:7400
```

Run the daemon as a user service (systemd `--user`), one per user and machine. Its socket directory is 0700.

## 3. Deploys and upgrades

1. Read the release's migrations. Migrations are append-only and run in their own transactions.
2. `xylemd migrate` against the primary, **before** rolling out: new servers may need the new schema, and migrations only add, so the old servers keep working on it. (`xylemd migrate` itself refuses a database that has a migration it doesn't know, i.e. an older binary.)
3. Roll the servers one at a time. On SIGTERM a server stops accepting, drains for up to 10 s, then closes. Wait for `/readyz` = 200 before moving on.
4. Then the caches, then the daemons (clients just reconnect).

`/healthz` means the process is up. `/readyz` means Postgres answers and the object store answers (server), or every upstream answers (cache, daemon). Load balancers should use `/readyz`.

## 4. Backups

Both stores are needed: Postgres holds history, the bucket holds content.

| What | How | Retention |
|---|---|---|
| PostgreSQL | Continuous WAL archiving / point-in-time recovery if your Postgres offers it (managed services do). Otherwise a nightly `pg_dump -Fc`. | 30 days |
| Object bucket | **Bucket versioning on**, with a lifecycle rule expiring noncurrent versions after 30 days. Objects are immutable and never deleted by Xylem (the GC sweep is not implemented, ADR-023), so versioning only guards against operator mistakes. | — |

```bash
pg_dump -Fc -d "$XYLEM_DB_URL" -f "xylem-$(date +%F).dump"     # nightly, then copy off the host
```

**Why this is consistent.** Xylem writes objects before any row refers to them, and never rewrites or deletes objects. A database backup taken at time T only refers to objects that existed at T, so it is consistent with the bucket at T or any later time. The reverse is not true: restoring the bucket to a point **older** than the database loses objects. `xylemd verify --objects` and `xylemd gc --dry-run` (the `missing` count) detect that.

## 5. Procedures

### 5.1 Restore drill (quarterly)

Tested against the dev stack on 2026-10-14 (6 repositories, ~15k objects: dump and restore took about a second; verify found 0 violations, GC 0 missing).

```bash
# 1. Restore the latest dump into a scratch database.
createdb -h db -U xylem xylem_drill
pg_restore -h db -U xylem -d xylem_drill --no-owner xylem-YYYY-MM-DD.dump
DRILL="postgres://xylem:…@db:5432/xylem_drill?sslmode=disable"

# 2. Check it against the live bucket (read-only).
xylemd verify <repo> --db "$DRILL" --store "$XYLEM_STORE" --sample 2000 --objects   # per repository
xylemd gc --dry-run --db "$DRILL" --store "$XYLEM_STORE"                             # "missing" must be absent

# 3. Optionally serve it on another port and clone from it.
xylemd server --db "$DRILL" --store "$XYLEM_STORE" --listen :7500 --http-listen :7501 &
xy clone localhost:7500 /tmp/drill-ws --sparse <dir> && xy -C /tmp/drill-ws log -n 5

# 4. Clean up.
dropdb -h db -U xylem xylem_drill
```

Record the date, dump size, restore time and results in the ops log.

### 5.2 Real restore

1. Stop the servers (or block writes at the load balancer).
2. Restore Postgres (PITR to just before the incident, or the latest dump) into a new database.
3. Run `xylemd verify <repo> --objects --sample 2000` for each repository. Fix the bucket first if objects are missing (restore noncurrent versions).
4. Point the servers at the restored database and start them. Caches and daemons need no action: objects are content-addressed, so their caches stay valid.
5. Lands made after the restore point are gone. Their authors still have them as local stacks in their workspaces and can `xy sync && xy land` again.

### 5.3 Integrity checks with `verify`

| Command | Checks | When |
|---|---|---|
| `xylemd verify <repo>` | Every changeset: body hash and columns, root tree re-derived from the parent and changes, core derived fields, seq order, refs, and that main's mainline equals the first-parent walk from its tip. Exit 1 on any violation (each printed as `VIOLATION …`). | After a restore, after an incident, before and after a major upgrade. |
| `… --sample N` | N random changesets instead of all | Weekly (e.g. `--sample 5000`) on large repositories. |
| `… --objects` | Also that each changeset's blobs and trees exist in the bucket | With every restore, and monthly. |
| `xylemd verify-import --repo <repo> --git <dir> (--all \| --sample N)` | An imported repository against its git source | After an import or a `--follow` gap. |
| `xylemd gc --dry-run` | Reachable objects missing from the bucket (warning), and the size of unreferenced objects | Monthly; the orphan size shows what the sweep (ADR-023) would reclaim. |

A violation means the database and the content disagree. Do not land on that repository until you understand it: compare with the latest backup, and file an issue with the `VIOLATION` lines.

### 5.4 Land contention

Symptoms: slow `xy land`, `Aborted: the tip kept moving`, and `xylem_land_attempts` p90 above 3. Fix: set `--land-batch-window 20ms` on the servers. The batcher lands every request in a batch with one ref update. Benchmark: ~400 lands/s becomes ~1,550 lands/s at 50 concurrent landers. With several servers, batches still race through the CAS but rarely lose.

### 5.5 Other incidents

| Symptom | Likely cause | Action |
|---|---|---|
| Server `/readyz` 503, clients get `Unavailable` / `Internal` | Postgres or the bucket is unreachable | Check the message in `/readyz`; fix the dependency. Nothing to repair afterwards. |
| Daemon `/readyz` 503, fetches `Unavailable`, `log`/`blame` still work | The cache or the server is down | Restart the cache. The daemon keeps serving what it has, and history calls go straight to the server. |
| `ResourceExhausted: cache tier overloaded` | `--max-inflight` upstream calls are running and 4× as many are already waiting | Raise `--max-inflight` or add a cache; check the server's latency. |
| `ResourceExhausted: rate limit for Blame: retry after Ns` | A caller hit `--rate-limits` | Expected for scripts that loop over files; raise the limit for that RPC if it hurts real use. |
| `Unauthenticated` / `PermissionDenied` spikes | Expired or wrong tokens, or probing | Check the identities in the logs (`xylem_rpc_duration_seconds_count{code="Unauthenticated"}`). |
| A workspace says `workspace is locked` | Another `xy` is running in it, or one crashed | The lock is an `flock` that dies with its process; wait, or check for a stuck `xy`. |

## 6. Monitoring and alerts

Scrape `/metrics` on every server, cache and daemon. Metrics:

| Metric | Type | Labels |
|---|---|---|
| `xylem_rpc_duration_seconds` | histogram | `method`, `code` |
| `xylem_land_attempts` | histogram | |
| `xylem_land_conflicts_total`, `xylem_merge_clean_total`, `xylem_merge_conflict_total` | counters | |
| `xylem_land_batch_requests` | histogram (land batcher) | |
| `xylem_rate_limited_total` | counter | `rpc` |
| `xylem_cache_hits_total`, `xylem_cache_misses_total` | counters | `tier` |
| `xylem_cache_bytes` | gauge | `tier`, `level` (memory, disk) |
| `xylem_upstream_inflight` | gauge | `tier` |

Suggested alerts (Prometheus rules; adjust thresholds to your traffic):

```yaml
groups:
- name: xylem
  rules:
  - alert: XylemNotReady            # page
    expr: probe_success{job="xylem-readyz"} == 0
    for: 2m
  - alert: XylemServerErrors        # page
    expr: |
      sum(rate(xylem_rpc_duration_seconds_count{code=~"Internal|Unavailable|DataLoss|Unknown"}[5m]))
        / sum(rate(xylem_rpc_duration_seconds_count[5m])) > 0.01
    for: 10m
  - alert: XylemLandSlow            # ticket
    expr: |
      histogram_quantile(0.99, sum by (le) (rate(xylem_rpc_duration_seconds_bucket{method="/xylem.v1.WriteService/Land"}[10m]))) > 5
    for: 15m
  - alert: XylemLandContention      # ticket: turn on or tune the land batcher (§5.4)
    expr: histogram_quantile(0.9, sum by (le) (rate(xylem_land_attempts_bucket[10m]))) > 3
    for: 15m
  - alert: XylemLandsAborted        # ticket
    expr: sum(rate(xylem_rpc_duration_seconds_count{method="/xylem.v1.WriteService/Land",code="Aborted"}[10m])) > 0
    for: 10m
  - alert: XylemCacheHitRatioLow    # ticket
    expr: |
      sum by (tier) (rate(xylem_cache_hits_total[30m]))
        / (sum by (tier) (rate(xylem_cache_hits_total[30m])) + sum by (tier) (rate(xylem_cache_misses_total[30m]))) < 0.8
    for: 1h
  - alert: XylemCacheOverloaded     # ticket
    expr: sum(rate(xylem_rpc_duration_seconds_count{code="ResourceExhausted",method=~"/xylem.v1.ObjectService/.*"}[5m])) > 0
    for: 10m
  - alert: XylemAuthFailures        # ticket
    expr: sum(rate(xylem_rpc_duration_seconds_count{code=~"Unauthenticated|PermissionDenied"}[5m])) > 1
    for: 15m
```

Also alert on Postgres (connections near `max_connections`, replication lag, disk) and on bucket errors, with your usual tooling.

**Logs** are JSON on stderr, one line per RPC (`method`, `repo`, `code`, `duration_ms`, `request_id`). The server returns the request id in the `x-request-id` header (a caller may send its own), so one id finds a call in the logs.

**Traces** (optional): with `--otlp-endpoint` on the servers and tiers and `XYLEM_OTLP_ENDPOINT` on clients, one trace follows a command from `xy` through the daemon and cache to the server.
