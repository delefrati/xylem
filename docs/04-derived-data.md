# 04 — Derived data

A changeset only records *what changed*. Everything else (full trees, ancestry indexes, history, blame) is **derived data**, computed from changesets by one generic mechanism.

## Principles

1. **Recursive definition.** The value of kind K at changeset C is a function of C's own data and K's values at C's parents (and, for keyed kinds, at the relevant earlier changesets).
2. **Immutable and versioned.** A value is keyed by `(kind, kind_version, changeset, key)`. Fixing a bug means bumping `kind_version`; old values are simply ignored and later garbage-collected.
3. **Eager or lazy per kind.** Eager kinds are computed inside the `CreateChangeset` transaction. Lazy kinds are computed on first request and memoized.
4. **Never recursive on the call stack.** History can be millions deep. Lazy computation walks backward iteratively to the nearest computed ancestor (or a root), then computes forward in order.
5. **Opaque to clients.** Clients only see API results, never derived-data formats, so formats can change freely (with a version bump).

## Framework (`internal/derived`)

```go
type Kind interface {
    Name() string                 // "blame"
    Version() int16               // bump on any format or semantic change
    Mode() Mode                   // Eager | Lazy
}

// Most kinds are "per changeset". Keyed kinds (blame) are per (changeset, key).
type Engine struct { /* meta.Store, store.ObjectStore, LRU of decoded values */ }

func (e *Engine) Get(ctx context.Context, k Kind, cs hash.ID, key []byte) ([]byte, error)
```

- Values ≤ 4 KiB go inline in `derived.value`. Larger values are stored as a `derived` object in the object store, with the id in `derived.value_ref`.
- Concurrent computation of the same value is fine because the results are deterministic. Writers use `INSERT … ON CONFLICT DO NOTHING`. A per-process `singleflight` collapses duplicate in-flight work.
- An in-process LRU caches decoded values. Since values are immutable, there is no invalidation.

## Kind 1 — `core` (eager, table `core_derived`)

Computed in the same transaction that inserts a changeset. It holds everything needed for checkout and ancestry.

### Root tree

```
root_tree(C) = apply(root_tree(parents[0]) or EMPTY_TREE, changes(C))
```

Incremental rewrite (`internal/treeops.Apply`) works like this:

1. Group changes by directory. Collect the set of directories touched, including all their ancestors up to the root.
2. Load the parent's trees along those paths only, via the object store and an LRU of decoded trees.
3. Apply **all** deletes first, pruning every directory they leave empty (recursively up to, but excluding, the root). Only then apply puts. This is what makes "replace directory `a/` with file `a`" valid within one changeset. Rewrite trees bottom-up.
4. Encode and store each rewritten tree. Untouched subtrees keep their ids.

Cost is O(changed paths × depth), independent of repository size. That is the property the whole design rests on.

### Generation number and first-parent depth

```
gen(C)      = 1 + max(gen(p) for p in parents)      ; roots: 1
fp_depth(C) = fp_depth(parents[0]) + 1               ; roots: 0
fp_parent(C)= parents[0]                             ; roots: NULL
```

### Skew-binary jump pointer (single pointer, O(log n) ancestor queries)

This is the Myers (1983) jump-pointer scheme, applied to the first-parent tree. Each node stores exactly one extra pointer.

```go
// computing jump(v) for a new changeset v with first parent p
if p == nil {
    jump(v) = v                                     // root points to itself
} else {
    j  := jump(p)
    jj := jump(j)
    if depth(p) - depth(j) == depth(j) - depth(jj) {
        jump(v) = jj
    } else {
        jump(v) = p
    }
}

// first-parent ancestor of v at depth d (d ≤ depth(v))
for depth(v) > d {
    if depth(jump(v)) >= d { v = jump(v) } else { v = fp_parent(v) }
}
return v
```

- Each loop step needs the `core_derived` row of `v`. Rows are immutable, so cache them aggressively (LRU of ~1M entries ≈ 150 MB is fine).
- **`IsFirstParentAncestor(a, b)`**: `depth(a) ≤ depth(b) && ancestorAtDepth(b, depth(a)) == a`.
- **`rev~N`**: `ancestorAtDepth(rev, depth(rev) − N)`.
- Property test: for random linear and branchy histories, compare against naive parent-walking.

### General ancestry and merge-base (full DAG)

`IsAncestor(a, b)`:
1. If `a == b`, return true. If `gen(a) ≥ gen(b)`, return false.
2. If `IsFirstParentAncestor(a, b)`, return true (fast path, the common case).
3. Otherwise do a BFS from b over all parents, pruning any node with `gen < gen(a)`. Return true if a is reached.

`MergeBases(a, b)` follows git's paint-down algorithm with generation numbers:
1. Use a priority queue ordered by `gen` descending (ties by id). Push a with flag A and b with flag B; keep a `flags` map per visited node.
2. Pop node n. If `flags[n]` has both A and B and not STALE: add n to candidates and set STALE on the flags to propagate. For each parent p of n: `flags[p] |= flags[n]` (including STALE); push p if it wasn't already queued or visited, or if its flags changed.
3. Stop when every node remaining in the queue is STALE.
4. Remove candidates that are ancestors of other candidates (via `IsAncestor`). Return the remaining set, sorted by `gen` descending, then by id.

Parity: must equal `git merge-base --all` as a set (see `10-testing.md`).

## Kind 2 — `mainline` (eager on ref update, table `mainline`)

This is the first-parent chain of `main` indexed by depth. It is maintained in the land/CAS transaction:

- **Fast-forward** (new tip's first-parent chain reaches the old tip): insert rows for depths `depth(old)+1 … depth(new)`, walking `fp_parent` from the new tip.
- **Non-fast-forward** (admin rewind): find `d` = the largest depth with `mainline[d] == ancestorAtDepth(new, d)` (binary search over d, since agreement is monotone), delete rows with `fp_depth > d`, then insert `d+1 … depth(new)` walking `fp_parent` from the new tip. This happens inside the same transaction as the ref CAS (`meta.CASRef` takes the rewind depth and the rows to insert). MVP only allows this via `xylemd admin set-ref --force`.

`IsOnMainline(C)` is a single indexed lookup.

## Kind 3 — path history (index-backed, no extra storage)

`changeset_files_path_idx (repo_id, path, seq DESC)` already is the path index. Log of path P (a file or directory) as seen from changeset C:

1. **Draft segment.** Walk `fp_parent` from C until reaching a mainline changeset M (drafts are short stacks; if no mainline changeset is reached within 10,000 steps, continue walking under step 3's cost limit). For each draft changeset in the segment, check its `changeset_files` for P.
2. **Mainline segment.** Query:
   ```sql
   SELECT cf.changeset_id, cf.seq
   FROM changeset_files cf
   JOIN mainline ml ON ml.repo_id = cf.repo_id AND ml.ref_name = 'main'
                   AND ml.changeset_id = cf.changeset_id
   WHERE cf.repo_id = $1
     AND (cf.path = $P OR (cf.path >= $P || '/' AND cf.path < $P || '0'))  -- '0' = '/'+1
     AND ml.fp_depth <= $depth_of_M
     AND cf.seq < $cursor_seq
   ORDER BY cf.seq DESC
   LIMIT $n;
   ```
   For directory queries several rows can belong to one changeset: use `SELECT DISTINCT ON (cf.seq) …` with the same `ORDER BY cf.seq DESC` (Postgres requires the `DISTINCT ON` expression to lead the `ORDER BY`; `seq` is unique per changeset). Paginate with the `seq` cursor.
3. **No mainline ancestor** (an unlanded branch imported from git, rare): fall back to walking `fp_parent` from C, checking `changeset_files` each step, with a cost limit. Return `ResourceExhausted` beyond 100k steps.

For P = repository root, the result is simply the first-parent log.

**Semantics:** a changeset "touches" P if its changes (relative to its first parent) include P or anything under P. For mainline merges, this attributes merged-in changes to the merge changeset. That matches `git log --first-parent -- P`.

## Kind 4 — `blame` (lazy, keyed by path, table `derived`)

**Definition:** `blame(C, P)` maps each line of file P at C to `(origin changeset, origin line number)`, following first-parent history only (`git blame --first-parent` semantics). Renames are not followed in the MVP.

**Storage key:** blame is stored only at changesets that **modify** P. To answer `blame(C, P)`, first find `L = last modifier of P at or before C` (path history, limit 1), then return `blame(L, P)`.

**Value encoding** (`kind = "blame"`, version 2; version 1 had the same encoding but was computed before the line diff compacted change runs):

```
uvarint(origin_count) ‖ id*                      // distinct origin changesets
uvarint(run_count) ‖ (uvarint(line_count) ‖ uvarint(origin_index) ‖ uvarint(origin_start_line))*
```

Runs cover the file's lines in order. `origin_start_line` is 1-based in the origin's version of the file.

**Computation** for L (a changeset that PUTs P):

```
prev := last modifier of P strictly before L on L's first-parent chain   (path history, limit 1)
if prev == nil or P is DELETEd at prev or P's type changed between prev and L (regular file ↔ symlink):
    blame(L) = all lines → (L, i)
else:
    b := blame(prev, P)                          // recursive by definition, iterative in code
    d := lineDiff(blob(prev, P), blob(L, P))     // Myers, see internal/diff
    unchanged lines inherit their origin from b; inserted or replaced lines → (L, line number in L)
```

Iterative evaluation: collect the chain `L, prev(L), prev(prev(L)) …` until a computed blame or a chain start is found, then compute forward and store each value. Chain length is the number of modifications of P, not total history.

- Binary files (a NUL byte in the first 8 KiB) return `FailedPrecondition: binary file`.
- Symlinks are blamed like files (the target is a one-line file). FILE ↔ EXEC changes don't reset blame.
- Lines are split after each `\n`. A final line without a trailing newline is still a line. Lines are compared as byte strings **including** their terminator, so `abc` (last line, no newline) differs from `abc\n`, matching git.

## Kind 5 — diff (computed, not stored)

- **Tree diff** `TreeDiff(A, B, pathFilter)`: walk both root trees together, skipping identical subtree ids, and emit `(path, old mode/blob, new mode/blob)`. When `A = parents[0](B)` and there's no filter, read `changeset_files` directly instead (it's free).
- **Line diff** is Myers' O(ND) algorithm (git's default) in `internal/diff`, followed by git-style compaction: runs of changed lines slide to canonical positions (as far down as they go, or the lowest position next to a change in the other file), so ambiguous lines such as blank lines are attributed as git does (ADR-021). It produces hunks with 3 lines of context for unified output. Line diff isn't stored; blame memoizes the expensive part.

## Adding a new derived kind later (checklist)

1. Implement `Kind` with a fresh name and version 1, and document its encoding here.
2. Decide eager or lazy. Eager kinds must be O(change size) and run inside the commit transaction.
3. Add a backfill command (`xylemd derive --kind X --from-seq N`) for existing history.
4. Add parity or property tests.

Examples for later: copy/rename detection, per-directory file counts, path-based file search, "last modified" per tree entry (for a fast `ls -l` in a future VFS).
