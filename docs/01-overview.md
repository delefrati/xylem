# 01 — Overview

**Xylem** is a centralized, server-first version control system for large monorepos, inspired by Anthropic's Scion ([article](https://www.anthropic.com/engineering/redesigning-source-control)), Google's Piper and Meta's Sapling/Mononoke.

> Naming: the project is **Xylem** (the plant tissue that carries water from the roots to every leaf, as the server feeds thin clients). The client binary is `xy`, the server/tier binary is `xylemd`, workspaces use `.xylem/`, and env vars use `XYLEM_`.

## The core idea

Git assumes every client is a full, offline-capable server. Most monorepo clients today are CI jobs, agents and sandboxes that are **always online**. Xylem drops the offline assumption, and that buys:

- a **thin client** that only fetches the trees and blobs it actually needs (sparse by default);
- a **smart server** that answers history questions (log, blame, diff, merge-base) and creates commits, rebases and merges;
- **content-addressed, immutable data**, so any tier can cache anything without coordination;
- **commits that cost O(size of change)**, not O(size of repo), and need no locks;
- **every query exposed as an API**, so many jobs never need a working copy.

## Goals (MVP)

1. Import an existing git repository (≥10k commits, merges included) and keep importing new git commits incrementally.
2. Serve history queries over gRPC: resolve revisions, log (repo/path), diff, blame, merge-base, ancestry, read file, list directory.
3. A CLI client (`xy`) that can clone (full or sparse), show status/diff, commit (server-side), and land onto `main` with automatic server-side rebase.
4. Correctness verified by **differential testing against git** (parity suite).
5. Land many changes concurrently from many workspaces without a global lock.

### MVP is "done" when

- A real repo (≥10k commits) imports and passes the tree, log, blame, diff and merge-base parity checks.
- `xy clone --sparse <dir>` produces a working tree byte-identical to git's for those dirs, fetching only objects under them.
- Twenty concurrent workspaces can each commit and `xy land` non-conflicting changes, all land, and `main` stays a valid first-parent chain.
- `make check` is green: lint, unit, integration and race tests.

## Non-goals (MVP)

- Offline work or distributed or peer-to-peer workflows.
- Git wire-protocol compatibility. Git is an import source and an optional export target only.
- Branches beyond `main` and release refs. Drafts are ref-less changesets.
- A virtual filesystem (FUSE), code review UI, directory ACLs, multi-region. These are planned in `13-future.md`.
- Submodules, LFS and Windows. Linux and macOS clients only.
- Spanner-scale storage. Postgres is the MVP metadata store.

## Success metrics (tracked, not gating)

| Metric | Target on a medium repo (~50k commits, ~1 GB history) |
|---|---|
| Import throughput | ≥ 200 commits/s on a laptop |
| `CreateChangeset` latency, 10-file change | p50 < 30 ms, p99 < 150 ms |
| Sparse clone of one top-level dir | proportional to that dir's size, not the repo's |
| Blame of a file with 1k-commit history, cold | < 2 s; warm < 50 ms |
| Parity suite | 100% on synthetic fixtures; ≥ 99% blame lines on real repos |

## Glossary

| Term | Meaning |
|---|---|
| **Object** | Content-addressed immutable data: a *blob* (file contents) or a *tree* (directory listing). |
| **Changeset** | The unit of history. Records parents, metadata and the list of files changed relative to its **first parent**. Created server-side. |
| **Draft** | A changeset not (yet) on `main`. It has no ref; it is addressed by id. |
| **Land** | Rebase a draft (or stack) onto `main`'s tip server-side and compare-and-swap `main` to the result. |
| **Mainline** | The first-parent chain of `main`. History and blame are defined along it. |
| **Derived data** | Anything computed from changesets: root trees, graph indexes, blame and so on. Each kind is defined recursively from parents' values. |
| **Workspace** | A client checkout directory: working files plus `.xylem/`. |
| **Sparse profile** | The set of directories a workspace materializes. |
| **Tier** | A process answering the object protocol: server, cache or daemon. All tiers speak the same RPCs. |
| **Seq** | A monotonically increasing log position assigned when a changeset is stored. |

## Document map

| Doc | Contents |
|---|---|
| `02-architecture.md` | Components, flows, repo layout, tech stack |
| `03-data-model.md` | Hashing, canonical encodings, Postgres schema, object store |
| `04-derived-data.md` | Framework plus root tree, graph, mainline, path history, blame |
| `05-api.md` | gRPC/Protobuf definitions and error model |
| `06-client.md` | CLI commands, workspace layout, checkout and status algorithms |
| `07-write-path.md` | CreateChangeset, rebase, land, conflicts, concurrency |
| `08-git-import.md` | git → Xylem importer |
| `09-fetch-and-caching.md` | Object fetch protocol, daemon and cache tiers |
| `10-testing.md` | Test strategy, git parity suite, fixtures, benchmarks |
| `11-dev-environment.md` | Tooling, Makefile, docker compose, config, CI |
| `12-decisions.md` | Architecture decision records (ADRs) |
| `13-future.md` | Post-MVP ideas |
| `roadmap.md` and `milestones/` | What to build, in what order, and how to verify it |
| `progress.md` | Running log kept by the implementing agent |
