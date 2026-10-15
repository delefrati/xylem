# M8 — Cache tiers (cache + daemon)

**Goal:** a regional cache and a per-node daemon speaking the same `ObjectService`, so many clients share objects and servers see only cache misses.
**Depends on:** M6.
**Read first:** `09-fetch-and-caching.md` (all), `02-architecture.md` (tiers, ports).

## Tasks

- [x] **M8.1 `internal/cache.Tier`.** Memory LRU plus an optional disk LRU (same key layout as the fs store), singleflight on misses, a miss-batching window (≤ 2 ms, ≤ 1024 ids), verification of upstream objects, an in-flight upstream limit with fast-fail, and write-through `PutObjects`.
- [ ] **M8.2 `xylemd cache`.** Flags `--upstream <server addr>`, `--memory-bytes`, `--disk-dir`, `--disk-bytes`. Serves `ObjectService` only; metadata RPCs return `Unimplemented`. Config validation enforces that the upstream is a server.
- [ ] **M8.3 `xylemd daemon`.** Listens on the Unix socket (per 09), upstream is a cache (or a server when `--allow-direct-upstream` is set, for dev). Implements `ObjectService` via `Tier` and **transparently proxies** `RepoService`/`WriteService` to the upstream server address (`--server`).
- [ ] **M8.4 Client support.** `server = "unix://…"` in `config.toml`. When connected to a daemon, the client may disable its local cache (`cache_max_bytes = 0`).
- [ ] **M8.5 Metrics and health** for both tiers (09 metric names), plus `/readyz` checking the upstream.
- [ ] **M8.6 Benchmarks** (`test/bench`, informational): 100 concurrent sparse clones of recent `main` revisions of `branchy` via daemon → cache → server vs direct. Record hit rates, server object reads and wall time in `progress.md`.

## Acceptance criteria

- The store contract suite also passes against a `Tier` (read path).
- Clone parity passes when the client goes through daemon → cache → server.
- In the benchmark, server object reads are < 5% of total object requests after warm-up.
- Killing the cache tier mid-benchmark gives clients clean `Unavailable` errors, and the server doesn't exceed its in-flight limit.
