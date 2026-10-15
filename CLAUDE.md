# CLAUDE.md — instructions for the implementing agent

You are implementing **Xylem**, a server-first version control system for large monorepos (Scion-inspired). The full design is in `docs/`. This file tells you how to work.

## Start of every session

1. Read `docs/progress.md` (where things stand, open questions, deviations).
2. Read `docs/roadmap.md` and find the **current milestone**: the first milestone, in the roadmap table's order, whose file in `docs/milestones/` has unchecked tasks. (Go by the table, not by file names: `M10` sorts before `M2`.)
3. Read that milestone file and the docs listed under "Read first". Don't read every doc every time; read what the task needs.
4. If `docs/progress.md` has an unanswered question addressed to the human that blocks the next task, ask it and stop.

## Work loop

For each unchecked task, in order:

1. Restate the task in one or two lines and identify the spec sections that govern it.
2. Implement it with tests (unit for pure logic; `-tags integration` for Postgres, S3 or gRPC).
3. Run `make check` (requires `make up`). Fix until green. **Never** skip, delete or weaken a test to get green.
4. Tick the checkbox in the milestone file and add a one-line entry to `docs/progress.md`.
5. **Don't commit or push** — the human reviews every change and commits it. Suggest a conventional commit message for the task, e.g. `feat(m2): incremental tree apply`.
6. **Stop** and wait for the human to review and commit. Continue with the next task only when told to.

At the **end of a milestone**: run the milestone's Verification commands, record results against each acceptance criterion in `progress.md`, write a short summary, and **stop for human review**. Also stop at any ⛔ marker inside a milestone.

## The spec is the source of truth

- `docs/` describes the intended design. If the spec is wrong, unclear or impossible:
  - **Small clarification** (doesn't change behaviour visible across packages): pick the sensible option, update the doc, and note it under "Deviations" in `progress.md`.
  - **Design decision** (new dependency, schema change, API change, different algorithm): add a **Proposed** ADR to `docs/12-decisions.md`, implement it if it's reversible, and flag it in `progress.md`.
  - **Hash-affecting change** (anything in `03-data-model.md` §Canonical encodings, ADR-004/006/015/017): **stop and ask the human.** Never change it on your own.
- Don't implement things from later milestones or from `13-future.md` early. Leave a `// TODO(Mx): …` if you see where they'll plug in.

## Engineering conventions (Go)

- Module layout per `docs/02-architecture.md`. Everything lives in `internal/`. `server` and `importer` depend on `repo`, not `meta`.
- `context.Context` is the first parameter of anything that does I/O. No package-level mutable state; wire dependencies in `cmd/`.
- Wrap errors with `fmt.Errorf("doing x: %w", err)`. Use typed sentinel errors (`var ErrNotFound = errors.New(...)`) and check them with `errors.Is`/`errors.As`.
- Log with `log/slog` structured fields. No `fmt.Println` outside CLI output code.
- Table-driven tests. Use `github.com/google/go-cmp/cmp` for diffs. Use `t.Parallel()` where safe. Use `testutil.NewDB(t)` for DB tests (fresh DB from template).
- Exported identifiers get doc comments in the packages that matter (`repo`, `derived`, `store`, `client`).
- Approved dependencies are the tech stack table in `docs/02-architecture.md`. Anything else needs a Proposed ADR.
- Bytes, not strings, for paths (ADR-015). Print non-UTF-8 paths quoted (`strconv.Quote`).
- Generated code in `gen/` is committed; run `make proto` after any `.proto` change.
- Performance matters where the spec says so: O(change size) on the commit path, batched I/O, no N+1 queries in loops.

## Commands

```bash
make tools         # pinned buf, golangci-lint, grpcurl, protoc plugins into ./bin
make up            # postgres + minio (docker compose)
make proto         # buf lint + generate
make build         # bin/xy, bin/xylemd
make test          # unit tests, no docker
make test-integration
make parity        # git parity suite on fixtures (M4+)
make e2e           # CLI end-to-end (M5+)
make check         # lint + unit + integration with -race + build
make fixtures      # synthetic git fixture repos (M3+)
make demo          # full local demo (M6)
```

## Never

- Change canonical encodings, golden vectors or applied migrations without human approval. Add new migrations instead of editing old ones once a milestone is closed.
- Disable `-race`, lint rules or tests to make CI pass.
- Commit secrets, large binaries or anything under `testdata/fixtures/` or `testdata/external/`.
- Rewrite unrelated code in a task. Keep diffs focused.

## When stuck

If you can't make progress after a reasonable attempt (e.g. 3 different approaches), write the problem, what you tried and the options under "Open questions" in `progress.md`, then move to an independent task if one exists, or stop and ask.
