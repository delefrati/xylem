# 07 — Write path: create, rebase, land

All writes happen on the server. The client uploads blobs and describes changes; the server builds changesets, trees and derived data.

## CreateChangeset (`internal/repo`, used by the RPC adapter in `internal/server` and by the importer)

1. **Canonicalize:** sort changes by path; normalize modes and sizes from the request.
2. **Validate request shape:** path syntax, op/mode combinations, limits (03 §Validation).
3. **Check blobs exist and sizes match:** `ObjectStore.Has(blob ids)`. If any are missing, return `FailedPrecondition` listing up to 20 missing ids in the message (the client re-checks with `HasObjects` anyway). Then verify declared sizes per 03 rule 3 (header-only read, in parallel).
4. **Load parents:** fetch the parents' `core_derived` rows, plus the rows of `jump(p0)` and `jump(jump(p0))` for the first parent `p0` (needed by the jump-pointer rule in 04). If any parent is missing → `NotFound`.
5. **Fill committer:** the caller's authenticated identity (M9.1), or the author when auth is off, with the time set to now (UTC, tz 0).
6. **Encode** the canonical body and compute the id.
7. **Fast idempotency check:** if `(repo, id)` exists, return it with `already_existed = true`.
8. **Apply changes** to the first parent's root tree (`treeops.Apply`). This also validates the structural rules (file/dir clashes, delete of missing paths, no-op puts). Store the new trees in the object store **before** the transaction. Orphan trees are harmless; GC is future work.
9. **Compute core derived:** gen, fp_depth, fp_parent, fp_jump (04).
10. **Transaction:**
    ```sql
    INSERT INTO changesets … ON CONFLICT DO NOTHING RETURNING seq;
    -- if no row returned: someone inserted it concurrently → rollback, return existing
    INSERT INTO changeset_files … (seq from above);  -- COPY for large change lists
    INSERT INTO core_derived …;
    COMMIT;
    ```
11. Return the `Changeset` proto.

There is no lock and no ref update. Concurrent creates of different changesets never interact. Concurrent creates of the same changeset are resolved by the PK.

**Performance budget:** the cost is O(changes × depth) object-store reads (mostly cache hits) plus one transaction. For change lists over 10k entries, use `pgx.CopyFrom` for `changeset_files`.

## Rebase (pure function plus inserts)

`RebaseStack(stack [X1..Xn], onto T)`:

```
newParent := T
for i, X := range stack:
    require X.parents[0] == (i == 0 ? X1.parents[0] : stack[i-1])   // well-formed stack
    require len(X.parents) == 1                                      // MVP: no merges in stacks
    base := X.parents[0]
    if base == newParent:                    // already based correctly (common for i > 0 when nothing changed)
        out[i] = X; newParent = X; continue
    changes', conflicts := rebaseChanges(X.changes, base, newParent)
    if conflicts: return conflicts, conflicting = X
    X' := CreateChangeset(parents=[newParent], author=X.author, message=X.message,
                          committer=(caller identity, attemptTime),   // one timestamp per Rebase/Land attempt
                          extras=X.extras + {"rebased-from": hex(X.id)}, changes=changes')
    out[i] = X'; newParent = X'
return out
```

`rebaseChanges(changes, base, onto)` for each change on path p, where `b = entry(base, p)`, `o = entry(onto, p)` and `x` = the change's target (or absent for a delete):

| Case | Result |
|---|---|
| `b == o` (p untouched since base) | keep the change |
| `o == x` (onto already has the same result) | drop the change (already applied) |
| both sides files and both changed the content, differently | three-way line merge (diff3, M7) for regular files within `--merge-max-bytes`: clean → PUT of the merged blob, else conflict `CONTENT` with `region_count`. Symlinks, binaries and larger files: conflict `CONTENT` (whole file) |
| `b` file, `o` absent (deleted on onto), `x` modified | conflict `DELETE_MODIFY` |
| `b` file, `o` modified, `x` delete | conflict `DELETE_MODIFY` |
| `b` absent, `o` present, `x` present, `o ≠ x` | diff3 against an empty base (M7); since both sides add at the same place this conflicts (`ADD_ADD` with `region_count`) unless the contents are identical |
| `o` is a directory (TREE) at p, or `x` would need p to be a directory where `o` has a file | conflict `FILE_DIRECTORY` |
| both sides files; only one side changed the mode and only the other changed the content | merge them: take the changed mode and the changed content (both sides' changes survive) |
| both sides changed the mode, to different modes | conflict `MODE` |

Mode and content are resolved independently: for each of the two, if only one side changed it, that side wins; if both changed it identically, keep it; if both changed it differently, conflict (`MODE` or `CONTENT`).

After per-path resolution, `treeops.Apply(onto tree, changes')` may still fail on structure (for example, onto added a file `a` while X adds `a/b`). Report that as `FILE_DIRECTORY`.

`entry(tree, p)` looks up a single path by descending the tree objects (cached). The cost is O(changes × depth), independent of how far `onto` has moved. This is why the design records changes rather than snapshots.

If all changes of an X are dropped (already applied), skip X entirely: don't create X', keep `newParent` unchanged, and add X's original id to `skipped`. If **every** entry is skipped, Land returns success with `landed` empty, `new_tip` = the current tip, and performs no CAS.

## Land

```
Land(stack, target="main", max_attempts=10):
  for attempt in 1..max_attempts:
      tip := SELECT changeset_id, version FROM refs WHERE repo_id = $r AND name = target
      rebased, conflicts := RebaseStack(stack, onto=tip)
      if conflicts: return conflicts                        // nothing landed
      newTip := last(rebased)
      BEGIN
        UPDATE refs SET changeset_id = newTip, version = version + 1, updated_at = now()
          WHERE repo_id = $r AND name = $target AND changeset_id = $tip        -- CAS
        if 0 rows: ROLLBACK; sleep(jitter(attempt)); continue
        INSERT INTO mainline (repo_id, ref_name, fp_depth, changeset_id) … for each rebased changeset
        INSERT INTO ref_log …
      COMMIT
      return landed=rebased, new_tip=newTip
  return Aborted("too much contention")
```

- **Retries.** Each attempt rebases the *original* stack onto the current tip with a fresh `attemptTime`, so rebased ids differ between attempts. Changesets from a failed attempt remain as harmless orphan drafts (cleaned up by future GC). The cost per attempt is O(stack changes × depth), so retries are cheap.
- **Committer of rebased changesets:** the caller's authenticated identity (each entry's author when auth is off) and the attempt's start time. Author fields are preserved.
- **Backoff:** `jitter(n) = rand(0, min(200ms, 5ms·2^n))`.
- **Post-merge testing model:** there is no pre-merge serialization and no merge queue lock. Correctness under concurrency is guaranteed only at the file level (the conflict table). Semantic breakage is caught by post-merge CI (see `13-future.md`). This follows Scion's lesson that a global merge-queue lock doesn't scale to agent-level throughput.

### Contention mitigation: the land batcher (M9.3)

When land QPS on one ref is high, CAS retries waste work. With `--land-batch-window` (e.g. `20ms`; default 0 = off) the server runs a **land batcher**: a single goroutine per (repo, ref) collects requests, rebases them sequentially onto the tip in memory, and performs one CAS for the whole batch. Multiple servers still race via CAS, but each server's land rate collapses into one CAS per batch.

- **Collecting.** A batch starts when the window since its oldest request has passed, when 100 requests wait, or as soon as as many requests wait as the previous batch had (when that was ≥ 2: the same landers are back, so waiting longer only adds latency).
- **Rebasing.** Requests are rebased in arrival order, each onto the tip as advanced by the ones before it. Trees and merged blobs go to the object store as usual; the rebased changesets are held in memory. A request whose rebase fails (unknown stack, cancelled caller) is answered with its error and leaves the batch.
- **Committing.** One transaction inserts every rebased changeset, moves the ref (CAS), writes the mainline rows and one ref-log entry (actor: the batch's callers, comma-separated). A lost CAS writes nothing; the whole batch is retried onto the new tip, and a request that runs out of its `max_attempts` gets `Aborted`.
- **Answers.** Conflicts and landings are answered only after the CAS succeeds, so a conflict is always against what actually landed (possibly an earlier request of the same batch). `new_tip` is the request's own top changeset (later requests of the batch may sit above it); `attempts` counts the batch's CAS attempts.
- **Metrics.** `xylem_land_batch_requests` (histogram of requests per batch).
- **Measured** (`make bench-land`: 50 concurrent landers through gRPC, 1,000 single-changeset lands that all rebase, local Postgres): ~400 lands/s and ~2.6 CAS attempts per land without the batcher; ~1,550 lands/s and 1.00 with a 20 ms window (3.8–4.0×).

## UpdateRef (admin)

- Without `force`: requires `IsFirstParentAncestor(old, new)` (fast-forward only) and updates `mainline` like land does.
- With `force`: rewinds `mainline` (see 04). Used only by `xylemd admin` and the importer.
- Ref names match `^[a-z0-9][a-z0-9._/-]{0,127}$`. `main` is created by the importer or by `xylemd admin init-repo`.

## Concurrency test requirements (M6 acceptance)

1. Twenty goroutines land concurrently, 50 times each; every land is a **single-changeset stack** touching paths disjoint from all other goroutines, and the test sets `max_attempts = 1000`. All succeed. `main` ends with exactly 1000 new mainline entries, and the final tree contains every change.
2. Two stacks editing the same file region: exactly one lands, the other returns `KIND_CONTENT`.
3. Kill-and-restart safety: a simulated failure between changeset insert and CAS leaves only an orphan draft; the ref and mainline stay consistent (`xylemd verify` passes).
4. Run under `-race`.
