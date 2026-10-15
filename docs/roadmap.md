# Roadmap

Milestones are sequential. Each has a task file in `docs/milestones/` with checkboxes, acceptance criteria and verification commands. **The MVP is M0–M6.**

| # | Milestone | Depends on | Est. (solo + agent) | Gate |
|---|---|---|---|---|
| M0 | [Bootstrap](milestones/M0-bootstrap.md) | — | 0.5 day | `make check` green, CI green |
| M1 | [Core objects and stores](milestones/M1-core-objects.md) | M0 | 2 days | Golden vectors approved by a human ⛔ |
| M2 | [Metadata and commit engine](milestones/M2-metadata-and-commits.md) | M1 | 4 days | 10k-changeset stress test plus `xylemd verify` |
| M3 | [Git import](milestones/M3-git-import.md) | M2 | 3 days | `verify-import --all` on all fixtures |
| M4 | [Read API and parity](milestones/M4-read-api.md) | M3 | 5 days | Parity suite passes |
| M5 | [Client read path](milestones/M5-client-read.md) | M4 | 4 days | Clone parity (full and sparse) |
| M6 | [Write path and land → **MVP**](milestones/M6-write-path.md) | M5 | 4 days | Concurrency suite plus MVP checklist |
| M7 | [Line-level merge](milestones/M7-line-merge.md) | M6 | 2 days | diff3 property tests |
| M8 | [Cache tiers](milestones/M8-cache-tiers.md) | M6 | 4 days | Hit-rate benchmark |
| M9 | [Hardening](milestones/M9-hardening.md) | M6 | 4 days | Auth, limits, land batcher |
| M10 | [Git export](milestones/M10-git-export.md) | M6 (M9 recommended) | 4 days | `verify-export`, fsck and round trip |

Total to MVP is about 3–4 weeks of focused work.

## Human checkpoints ⛔

The agent **stops and waits** for a human at:

1. **End of M0:** confirm the module path, project name and tool choices.
2. **M1.8:** review the golden vectors. They freeze the hash-affecting encodings; after approval, any change needs an ADR.
3. **End of every milestone:** review `docs/progress.md`, the acceptance results and any Proposed ADRs.
4. **Any deviation** from a spec that changes an encoding, a schema already migrated in a shared environment, or a public proto field number.

## Critical path and risk

| Risk | Where | Mitigation |
|---|---|---|
| Encoding mistakes discovered late | M1 | Golden vectors plus fuzz, human review at M1.8 |
| Incremental tree rewrite bugs | M2 | Brute-force property test on every change list |
| git edge cases (octopus, encodings, type changes) | M3 | `edge` fixture plus `verify-import --all` |
| Blame or log semantics drift from git | M4 | Parity suite with exact sequences |
| Checkout correctness (modes, symlinks, partial writes) | M5 | Clone parity, atomic writes, crash-order rules |
| Races in land and mainline | M6 | CAS-in-transaction, 20-way concurrency test under `-race` |
| Exported git history differs from Xylem, or diverges | M10 | `verify-export`, determinism and round-trip tests, CAS on the git branch |

## Definition of done (every task)

- Code plus tests written. Unit tests for pure logic; integration tests for anything touching Postgres, S3 or gRPC.
- `make check` passes (needs `make up`).
- Spec docs updated if behaviour differs or was clarified, with an ADR if it's a decision.
- The task's checkbox is ticked in its milestone file, with a one-line entry in `docs/progress.md`.
- One conventional commit per task (or a few, if the task is large), made by the human after review; the agent suggests the message.
