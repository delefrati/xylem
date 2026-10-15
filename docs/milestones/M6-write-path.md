# M6 — Write path and land (completes the MVP)

**Goal:** commit from a workspace, rebase stacks server-side, and land concurrently onto `main` with file-level conflict detection.
**Depends on:** M5.
**Read first:** `07-write-path.md` (all), `06-client.md` §Commit and §Stack discovery, ADR-007, 008, 012, 019.

## Tasks

- [x] **M6.1 `PutObjects`.** Server tier: decode, verify hash and size limits, `store.Put`, count stored and already-present. Clients upload **blobs only**, because trees are always built server-side. Reject any uploaded object whose kind isn't `blob` with `InvalidArgument`.
- [x] **M6.2 `CreateChangeset` RPC.** Adapter over `repo.CreateChangeset`. Committer = author for now (M9 sets it from auth). Map typed validation errors to `InvalidArgument` and missing blobs to `FailedPrecondition` with up to 20 ids in the message (05 §Error model).
- [x] **M6.3 Client `add` and `commit`.** Per 06: gather changes, upload missing blobs (`HasObjects` → `PutObjects` batches packed up to ~1 MiB per message), create the changeset, move `@`, update dirstate and `state.added`. Author resolution order per 06.
- [ ] **M6.4 `repo.RebaseStack`.** Per 07: per-path rules (file-level conflicts), structural conflict mapping, skipping already-applied changes, `rebased-from` extra, and stack well-formedness checks. Unit-test every row of the conflict table plus FILE_DIRECTORY.
- [ ] **M6.5 `repo.Land`.** CAS loop with jittered backoff, mainline rows and ref_log in the same transaction, `max_attempts`, and metrics (`xylem_land_attempts`, `xylem_land_conflicts_total`).
- [ ] **M6.6 Rebase and Land RPCs.** Adapters returning conflicts in the response body (not as errors).
- [ ] **M6.7 Client `stack`, `sync` and `land`.** Per 06, with exit code 3 on conflicts and a printed conflict table. After a successful land or sync, check out the new top (no refetch of unchanged objects).
- [ ] **M6.8 Concurrency suite.** The four scenarios in 07 §Concurrency test requirements, as integration tests under `-race`.
- [ ] **M6.9 Demo and MVP checklist.** Implement `make demo` (11). Run the MVP "done when" list from `01-overview.md` and record each item's evidence (command plus result) in `progress.md`. Update `README.md` with a 5-minute quickstart.

## Acceptance criteria (MVP)

- Every item of `01-overview.md` §"MVP is done when" is demonstrated and recorded.
- The concurrency suite passes 5 consecutive runs (`-count=5 -race`).
- `xylemd verify --all` passes after the concurrency suite.
- e2e: two workspaces edit different files → both land. They then edit the same file → the second gets exit code 3 and the workspace is untouched.

## Verification

```bash
make up && make check && make parity && make e2e
go test -tags integration -race -count=5 -run TestLandConcurrency ./internal/repo/...
make demo
```

⛔ **Stop: MVP complete. Human review before M7+.**
