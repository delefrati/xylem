# M7 — Line-level merge (diff3)

**Goal:** rebases and lands succeed when concurrent edits touch different regions of the same file.
**Depends on:** M6.
**Read first:** `07-write-path.md` §Rebase conflict table, ADR-012.

## Tasks

- [x] **M7.1 `internal/merge.Merge3(base, ours, theirs []byte) (merged []byte, conflicts []Region, err)`.** diff3 over line sequences using `internal/diff` (base→ours, base→theirs), merging non-overlapping hunks and flagging overlapping ones as conflicts unless both sides made the identical change. Preserve the trailing-newline state. Binary files are always a conflict.
- [x] **M7.2 Property tests.** Non-overlapping random edits merge cleanly and the result contains both edits. Merge3 is symmetric (ours ↔ theirs) on clean merges. `Merge3(b, b, x) == x` and `Merge3(b, x, x) == x`.
- [x] **M7.3 Integrate into `RebaseStack`.** For CONTENT (and ADD_ADD with an empty base): on a clean merge, store the merged blob and emit a PUT. Otherwise return the conflict with the region count. Add a server option `--merge-max-bytes` (default 8 MiB per side) above which content conflicts stay file-level.
- [x] **M7.4 Tests and metrics.** Update the concurrency suite scenario 2 (same file, *different* regions → both land; same region → conflict). Add `xylem_merge_clean_total` and `xylem_merge_conflict_total`. Update ADR-012 to note that M7 is done.

## Acceptance criteria

- Property tests pass, plus 5 minutes of fuzzing of `Merge3` with no panics.
- The updated concurrency suite passes under `-race`.
