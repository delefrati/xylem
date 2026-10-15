# Xylem

A server-first version control system for large monorepos, built for always-online clients (CI jobs, agents, sandboxes). It's inspired by Anthropic's [Scion](https://www.anthropic.com/engineering/redesigning-source-control), Google's Piper and Meta's Sapling.

- **Thin client, smart server.** History, blame, diff and merges are computed server-side and exposed as gRPC APIs.
- **Sparse by default.** Clients fetch only the trees and blobs they ask for.
- **Content-addressed everything.** Any tier can cache anything.
- **O(change) commits.** No locks; `main` moves by compare-and-swap.

> **Status:** the MVP (M0–M6) is implemented: git import, the read and write APIs, the `xy` client and concurrent land. See [`docs/roadmap.md`](docs/roadmap.md) and [`docs/progress.md`](docs/progress.md).

## How this repo is meant to be built

This repository starts as a **plan**: specs, milestones and agent instructions. An AI coding agent (Claude Code in VS Code) implements it milestone by milestone, with a human reviewing at checkpoints.

### Setup

1. Create an empty GitHub repo (e.g. `xy`) and copy these files into it.
2. `git init && git add . && git commit -m "docs: initial plan"`.
3. Install Go (latest stable), Docker with Compose v2, and git.
4. Open the folder in VS Code with the Claude Code extension.
5. Start a session and run **`/next`**, or say: *"Read CLAUDE.md and start the current milestone."*
6. The agent stops at the end of each milestone (and at ⛔ checkpoints). Review `docs/progress.md`, the commits and the acceptance results. Then run `/next` again, or `/review-milestone` for an independent check first.

### Tips for driving the agent

- Keep one milestone per session if the context gets long. `progress.md` carries state between sessions.
- Answer open questions in `progress.md` directly in the file, then run `/next`.
- For high-stakes milestones (M1 encodings, M6 concurrency), run `/review-milestone` before approving.

## Docs

| | |
|---|---|
| [01 Overview](docs/01-overview.md) | Goals, MVP definition, glossary |
| [02 Architecture](docs/02-architecture.md) | Components, flows, code layout, stack |
| [03 Data model](docs/03-data-model.md) | Hashing, encodings, Postgres schema |
| [04 Derived data](docs/04-derived-data.md) | Trees, ancestry, path history, blame |
| [05 API](docs/05-api.md) | gRPC/Protobuf |
| [06 Client](docs/06-client.md) | CLI and workspace |
| [07 Write path](docs/07-write-path.md) | Commit, rebase, land |
| [08 Git import](docs/08-git-import.md) | Importer and mirror sync |
| [09 Fetch and caching](docs/09-fetch-and-caching.md) | Protocol and tiers |
| [10 Testing](docs/10-testing.md) | Git parity suite and fixtures |
| [11 Dev environment](docs/11-dev-environment.md) | Make, compose, config |
| [12 Decisions](docs/12-decisions.md) | ADRs |
| [13 Future](docs/13-future.md) | Post-MVP |

## Getting started

Needs Go (see `go.mod`), Docker with Compose v2, and git. See [11 Dev environment](docs/11-dev-environment.md).

```bash
make tools        # pinned buf, golangci-lint, grpcurl and protoc plugins into ./bin (once)
make up           # postgres on :54329, MinIO on :59000 with the xylem-objects bucket
make proto        # regenerate gen/ after editing proto/ (generated code is committed)
make check        # lint + unit and integration tests with -race + build
bin/xy version
bin/xylemd version
make down         # stop services (data volumes are kept)
```

`make help` lists every target.

## Quickstart (5 minutes)

`make demo` does all of this in one go, against a small fixture. By hand, with the 2,000-commit `branchy` fixture:

```bash
make tools up build                    # pinned tools, Postgres + MinIO, bin/xy and bin/xylemd
bin/xylemd migrate
make fixtures                          # synthetic git repos in testdata/fixtures/
bin/xylemd import --git testdata/fixtures/branchy --branch main
make run-server                        # leave running: gRPC :7400, HTTP :7401
```

In another terminal:

```bash
export PATH=$PWD/bin:$PATH XYLEM_AUTHOR="Your Name <you@example.com>"
xy clone localhost:7400 /tmp/ws --sparse internal/api,docs   # only these dirs (and root files)
cd /tmp/ws
echo hi >> docs/file007.txt
xy status && xy diff                   # local, from cached blobs
xy commit -m "say hi"                  # a draft on the server; no push
xy land                                # rebase onto main server-side, CAS main, check out the tip
xy log --oneline -n 3
xy blame docs/file007.txt
xy cat -r main~5 internal/core/file002.txt   # out-of-scope files and old revisions via the API
```

Everything the client shows comes from the server's APIs; see [api-examples.md](docs/api-examples.md) for the same calls with grpcurl.
