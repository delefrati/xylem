# Progress log

Maintained by the implementing agent. Newest entries first within each section.

## Status

- **Current milestone:** M4 — Read API and parity
- **Last checkpoint approved by human:** M3 — 2026-10-14 (M2, M1, golden vectors and M0 approved 2026-10-14)
- **Blocked on:** —

## Open questions (for the human)

- [x] Q-003 (M1.8 ⛔): Approve the golden vectors in `testdata/golden/README.md`? Approval freezes the encodings; later changes need an ADR. → answer: approved 2026-10-14 (incl. `blob_hello` = "hello\n", synthetic sizes in `tree_sizes`, merge changes vs first parent)
- [x] Q-001 (M0.1): GitHub owner for the module path (`github.com/<owner>/xylem`)? → answer: `delefrati` (module `github.com/delefrati/xylem`)
- [x] Q-002 (M0.5): License (default MIT)? → answer: MIT

## Deviations from spec

<!-- - YYYY-MM-DD M2.4: <what changed, why, doc updated: 04-derived-data.md> -->
- 2026-10-14 M4.4: GetChangesets returns NotFound if any requested id is unknown (the response has no "missing" field); ResolveRevision requires a revision (no default); UpdateRef records the actor as `rpc:<request id>` until M9 auth; `on_mainline` refers to main.
- 2026-10-14 M4.3: GetObjects packs objects up to ~1 MiB per message (05 §Conventions, the wire contract) rather than the ~4 MiB in the M4.3 task text; frames larger than a quarter of --object-cache-bytes are not cached; ObjectService checks that the repo exists (objects themselves are shared across repos in the MVP).
- 2026-10-14 M4.2: revision details beyond 03: `~`/`^` without a count mean 1 and `^0` is the revision itself (as in git); a name that is both a ref and a hex prefix resolves as the ref; `git:` accepts 4–40 hex characters; `@` is rejected server-side with InvalidArgument. Added meta.ResolveGitPrefix and candidate lists in ambiguity errors (repo.ResolveHex/ResolveGitSHA).
- 2026-10-14 M3.7: verify-import also reports commits that are not imported and checks that main is at the mapped branch tip; content falls back to re-hashing the git blob when a git_blobs row is missing. The git_blobs migration itself landed in M3.4.
- 2026-10-14 M3.6: `xylemd import` also takes --db, --store and --workers, and creates the Xylem repo if missing (`--repo`, default "default"). xylemd now runs commands under a SIGINT/SIGTERM-cancelled context so an interrupted import checkpoints and resumes.
- 2026-10-14 M3.4: `migrations/0002_git_blobs.sql` (verbatim from 08) lands with the pipeline, which fills it, rather than in M3.7. The importer talks to the database through new `repo` methods (OpenOrCreate, GitSHAMap, ImportState/SetImportState, GitBlobs/RecordGitBlobs, Changeset), keeping importer → repo (not meta). Blob dedup across workers uses x/sync/singleflight. Blob existence is checked per blob (Has/Put) inside the singleflight rather than in per-commit batches.
- 2026-10-14 M3.3: mapping details beyond 08: 100664 (old git) maps to FILE; git changes invisible in Xylem (same blob, same mapped mode) are dropped instead of becoming no-op PUTs; type changes involving a submodule map to DELETE (file→gitlink) or PUT (gitlink→file); `git-extra-submodules` lists the gitlink paths each commit touched. Warnings for skipped submodules are emitted (deduplicated per path) by the pipeline from `Mapped.Submodules`.
- 2026-10-14 M3.2: gitsource rejects sha256 repositories (git_sha is 20 bytes); DiffTree runs one diff-tree process per commit (the importer parallelizes); added LsTree (for verify-import), IsAncestor and FirstParentChain helpers. Commit headers other than tree/parent/author/committer/encoding (gpgsig, mergetag) are not kept.
- 2026-10-14 M3.1: tests get fixtures through `testutil.GitFixture(t, profile)`, cached in `~/.cache/xylem-fixtures/<hash of generators>/` (atomic rename), so CI needs no `make fixtures` step; `make fixtures` still fills `testdata/fixtures/`. `edge` needs `openssl` (deterministic 11 MiB binary). G204 (subprocess with variable args) is suppressed with `//nolint:gosec` at each git/fixture call site (ADR-010: shelling out to git is the design) rather than in config — for human review.
- 2026-10-14 M2.9: under `-race` (as in `make check`) TestStress runs 1,000 changesets instead of 10,000 to keep `make check` fast; the milestone verification command (no -race) runs the full 10,000.
- 2026-10-14 M2.8: verify also checks body ↔ id ↔ denormalized columns and changeset_files (body is the source of truth) and that refs point at existing changesets. `--objects` checks what each changeset introduces (PUT blobs, root tree, trees along changed paths); by induction that covers every reachable object. Added meta FilesFor/ListChangesets/SampleChangesets/ListRefs/MainlineRange for batched reads (no N+1).
- 2026-10-14 M2.7: only `main` keeps a mainline (`repo.MainRef`, the MVP scope in 03); the forced-update agreement depth compares `AncestorAtDepth` of the old and new tips (equivalent to `mainline[d]`, since mainline is the old tip's chain). CreateChangeset stores the empty tree object whenever a root tree is empty, so clients can fetch it. `xylemd admin set-ref` takes a full id or unique ≥8-hex prefix (`repo.ResolveHex`) until M4's revision syntax.
- 2026-10-14 M2.6: `CreateOptions` also has `TrustSizes` (03 rule 3 importer option); `Now` lives on `repo.Options` (one clock per Repo). Root changesets in an empty repo need no `AllowRoot` (03 rule 1). Added `object.PeekHeader` (streaming header-only read for rule 3). The `git-sha` extra must be 40 lowercase hex (it fills the `git_sha` column).
- 2026-10-14 M2.5: `derived.Source` has `Core` and `Parents` (BFS and paint-down need all parents, which core_derived lacks); added `meta.Store.GetParents` (selects only the parents column). `ComputeCore` takes (id, parents, rootTree) rather than a whole changeset. Noted in code: with generation-ordered paint-down, step 4 (remove redundant) never drops anything; kept per spec and tested directly.
- 2026-10-14 M2.4: `treeops.Apply` returns `[]NewTree{ID, Stored, Tree}` (stored bytes plus the decoded tree, so callers can store and cache) and also rejects duplicate paths (`ErrDuplicatePath`, found by the property test; changeset validation already rejects them, this is defense in depth). Deleting a directory path or through a file is `ErrDeleteMissing`.
- 2026-10-14 M2.3: no standalone `MainlineRewind`; rewinds go through `CASRef(…, MainlineUpdate{Rewind, KeepThrough, Rows}, …)` so mainline changes always share the ref CAS transaction (03 §Invariants, 04 §Kind 2). `author_time`/`committer_time` columns are clamped to years 1–9999 (body stays exact; golden vector `changeset_non_utf8` uses MaxInt64). Store methods take a `RepoID`; batch getters return maps.
- 2026-10-14 M2.2: 10-testing.md named the per-test template `xylem_template` in step 3 but `xylem_tmpl_<hash>` in step 2; uses the hashed name (built as `…_building`, renamed when complete); doc updated: 10-testing.md
- 2026-10-14 M2.1: the migrations live in a root Go package `migrations` (embed.FS needs a Go file next to the .sql files); `schema_migrations(version, applied_at)` per ADR-018; `Migrate` refuses to run if the DB has a migration the binary does not know. Two targeted `//nolint:gosec` (G101) on the documented dev/test default DB URLs.
- 2026-10-14 M1.5: `ValidateShape` also checks `parent_count ≤ 16` (shape part of rule 1) and that extra keys are non-empty UTF-8 (proto `map<string, bytes>` cannot carry invalid UTF-8 keys); `Summary()` also drops a trailing `\r` and replaces NUL with U+FFFD (Postgres TEXT cannot store NUL). Not hash-affecting.
- 2026-10-14 M1.5: new package `internal/canon` (shared canonical varint/bytes/id reader used by trees and changesets) instead of duplicating it; doc updated: 02-architecture.md
- 2026-10-14 M1.4: 10-testing.md object property "any single-byte mutation of a stored frame fails Verify" is unachievable with standard zstd (e.g. raising the window descriptor decodes to identical bytes). Reworded to "no mutation verifies with different content", which is the integrity guarantee; doc updated: 10-testing.md
- 2026-10-14 M0.4: **ADR-020 (Accepted 2026-10-14)** — upstream `minio/minio`/`minio/mc` images are no longer pullable; switched to pinned `pgsty/minio`/`pgsty/mc`.
- 2026-10-14 M0.4: `make up` runs `docker compose run --rm minio-init` instead of `compose wait` (which can't see already-exited one-shot containers); doc updated: 11-dev-environment.md
- 2026-10-14 M0.4: `make reset-db` description said the `xy` DB; it's `xylem`; doc updated: 11-dev-environment.md
- 2026-10-14 M0.2: client CLI lives in `cmd/xy` (M0.2, binary name), not `cmd/xylem` as the layout tree said; doc updated: 02-architecture.md

## Milestone results

### M3 — 2026-10-14

Approved by the human 2026-10-14. Verified on 264455e (+ reset-db fix): `make up && make fixtures && make check` → exit 0; `bin/xylemd import --git testdata/fixtures/edge --branch main && bin/xylemd verify-import --git testdata/fixtures/edge --all` → 27 commits, 368 files, 0 mismatches.

| Acceptance criterion | Evidence | Result |
|---|---|---|
| `verify-import --all` passes for tiny, edge, branchy | Binary against the dev DB: tiny 10 commits / 34 files, edge 27 / 368, branchy 2,000 / 567,366 — 0 mismatches each; same in integration tests | ✅ |
| Re-running import on an up-to-date repo is a no-op (0 new changesets) in < 2 s | `imported 0 new commits` for each fixture, 0.01 s wall time | ✅ |
| `main` points at the mapped tip and `xylemd verify --all` passes | verify-import checks main = mapped tip; `xylemd verify <repo> --all --objects` → 0 violations for all three | ✅ |
| Import throughput logged for branchy (and the external small repo) | branchy 605 commits/s (/tmp store), 139/s (FS store on the slow project drive); spf13/cobra 623 commits/s, verify-import 0 mismatches | ✅ |

**Summary:** git repositories import through a parallel, ordered pipeline (gitsource → mapping → blob upload → committer) that is resumable (checkpoints every batch and on interrupt, preloaded sha map, git_blobs table) and incremental (`--follow`, `--since`). Every edge case from 08 is in the `edge` fixture and verified tree for tree; a real repository (cobra) also matches exactly. A data race between the producer and the committer was found by `-race` and fixed. Worth a look: G204 `//nolint` at each git call site (ADR-010); the `git_blobs` migration landed with the pipeline; throughput depends on fsync speed of the FS store's disk.

### M2 — 2026-10-14

Approved by the human 2026-10-14; CI run 38643279121 green on 15031cd. Verified on 0022fcf: `make up && make check` → exit 0 (all packages pass under `-race`, including the 1,000-changeset race variant of TestStress); `go test -tags integration -run TestStress ./internal/repo/... -v` → PASS.

| Acceptance criterion | Evidence | Result |
|---|---|---|
| All unit and integration tests pass under `-race` | `make check` green: 13 packages, 0 lint issues | ✅ |
| Stress test passes, `verify` reports zero violations | 10,000 changesets (5.4% merges, 1,155 fast-forwards, ~1,000 files), `Verify{Objects}` → 0 violations in 3.4 s | ✅ |
| `CreateChangeset`, 10-file change on a 1,000-file tree: p50 < 30 ms (report it) | p50 1.96–2.1 ms, p99 2.3–2.6 ms (no -race, local Postgres, mem store; 13.7 ms p50 under -race) | ✅ |
| 10 concurrent identical `CreateChangeset` → one row, nine `already_existed`, no error | `TestCreateIdempotentAndConcurrent` (same id and seq for all ten) | ✅ |

**Summary:** changesets are created in-process with incremental trees (property-tested against from-scratch builds), eager core derived data (property-tested against naive ancestry and merge-base on random DAGs), Postgres metadata in one transaction, and refs whose mainline is maintained in the CAS transaction. `xylemd migrate`, `admin init-repo`, `admin set-ref` and `verify` work end to end; verify detects 13 kinds of tampering. Deviations worth a look: no standalone `MainlineRewind` (rewinds go through `CASRef`), timestamp columns clamped to years 1–9999 (body exact), `meta.GetParents` and batch listing methods added, only `main` keeps a mainline, and TestStress runs 1,000 changesets under `-race`.

### M1 — 2026-10-14

Verified on 21a4511: `make test && make test-integration && make fuzz` → exit 0; coverage command from the milestone file below.

| Acceptance criterion | Evidence | Result |
|---|---|---|
| Golden vectors approved by the human (record the date) | Approved 2026-10-14 (Q-003); 15 vectors in `testdata/golden`, ids cross-checked with Python `blake3`, bodies parsed by an independent spec-based decoder | ✅ |
| `make fuzz` runs 30 s per target with no crashes | FuzzDecodeChangeset 3.09M execs, FuzzDecodeTree 3.22M, FuzzObjectDecode 3.32M, FuzzValidPath 3.55M; all PASS | ✅ |
| Store contract suite passes for mem, fs and s3 | `storetest.Run`: 9 subtests each for Mem, FS (unit) and S3 against MinIO (`-tags integration`, incl. 24 MiB multipart) | ✅ |
| Coverage of hash, paths, object, changeset ≥ 90% | hash 100.0%, paths 100.0%, object 98.4%, changeset 99.4% (store 88.7% incl. S3, not gated) | ✅ |

**Summary:** the hash-affecting encodings (ids, headers, trees, changesets) are implemented exactly as specified and frozen by approved golden vectors; decoders accept only canonical input and are fuzzed; the three object stores share one contract suite. Deviations to note: `internal/canon` package added; the object mutation property was reworded to content integrity (zstd frames are not canonical); `ValidateShape`/`Summary` have small additions forced by proto and Postgres constraints. None is hash-affecting.

### M0 — 2026-10-14

Verified on a fresh `git clone` of 3e549f4: `make tools && make up && make proto && make check` → exit 0 (23 s with warm caches); `git status --porcelain` empty afterwards.

| Acceptance criterion | Evidence | Result |
|---|---|---|
| `make tools && make up && make proto && make check` passes on a clean clone | Clean-clone run above; golangci-lint 0 issues, `go test -race -tags integration ./...` ok, build ok | ✅ |
| `bin/xy version` and `bin/xylemd version` print the version info | `xy 3e549f4 (commit 3e549f4, go1.27.2 linux/amd64)`, same for `xylemd`; covered by `TestVersionCommand` | ✅ |
| `buf lint` passes on the proto files | `buf lint` (STANDARD) clean on the 4 protos copied verbatim from 05-api.md | ✅ |
| CI is green on the first push | GitHub Actions run 38611733577 on bb60187: lint, unit, integration, build, proto freshness all success (~5 min, cold tool cache) | ✅ |

**Summary:** the repo builds, lints, tests, generates protobuf code (committed, freshness-checked), runs Postgres 17.11 and MinIO locally, and has CI. Approved by the human on 2026-10-14, including **ADR-020** (pinned `pgsty/minio` and `pgsty/mc` replace the unavailable upstream MinIO images).

<!--
### M1 — YYYY-MM-DD
| Acceptance criterion | Evidence | Result |
|---|---|---|
-->

## Measurements

<!-- - YYYY-MM-DD: CreateChangeset 10 files on 1k-file tree: p50 X ms, p99 Y ms (M2.9) -->

- 2026-10-14: import external small repo spf13/cobra @adbc881 (1,106 commits, 114 merges, 1,861 blobs, 30.6 MB): 1.8 s ≈ 623 commits/s; verify-import --all 0 mismatches (50,483 files); verify 0 violations (M3)
- 2026-10-14: import branchy via the binary into the dev DB with `fs://./data/objects` on the project drive: 14.4 s ≈ 139 commits/s — the FS store fsyncs every object and that drive is slow to fsync; the same import with the store in /tmp: 3.3 s ≈ 605 commits/s (M3)
- 2026-10-14: import branchy (2,000 commits, 5,413 blobs, 7.2 MB): 3.4 s ≈ 595 commits/s (local, mem store, 8 workers, no -race) (M3.4)
- 2026-10-14: stress 10,000 changesets on a ~1,000-file tree (5.4% merges, 1,155 fast-forwards): created in 15.6–21.3 s (1.6–2.1 ms/changeset over 4 runs; one outlier run right after `make check` took 60.6 s / 6.1 ms), verify --all --objects in 3.4 s, 0 violations (M2.9)
- 2026-10-14: CreateChangeset 10 files on 1k-file tree: p50 2.0 ms, p99 2.6 ms (local Postgres, mem store, no -race; 13.7 ms p50 under -race) (M2.6)
## Log

<!-- - YYYY-MM-DD M0.2: skeleton dirs + cobra roots (commit abc1234) -->
- 2026-10-14 M4.4: ResolveRevision, GetChangesets (request order, ≤1000, include_changes, on_mainline via batched mainline lookup), ListRefs, UpdateRef (admin semantics); internal/server/convert.go (modes, ops, signatures with nanos = 0 enforced, changes, changesets) with round-trip tests; repo.Changesets/MainlineDepths/ListRefs
- 2026-10-14 M4.3: ObjectService GetObjects (≤1024 ids, 16 parallel reads, byte-bounded LRU of stored frames, ~1 MiB packing, large objects alone, missing in the final message) and HasObjects (cache first, request order); PutObjects → Unimplemented until M6
- 2026-10-14 M4.2: internal/revision Parse/String (refs, ≥8-hex prefixes, git:<sha>, @, chained ~N/^N) and Resolve against repo (jump pointers for ~N, parent lists for ^N, ambiguity → InvalidArgument with up to 5 candidates)
- 2026-10-14 fix(m3): CI integration failed on the M3 push — make-fixture-repo.sh wrote ../msg.txt (outside its dir, into the shared fixture cache), so concurrent cold-cache generations collided; the message now goes through stdin; added a 4-way concurrent generation test
- 2026-10-14 M4.1: internal/server (Server with Unimplemented services, 80 MiB limits, reflection; interceptors: request id, JSON access log, xylem_rpc_duration_seconds, panic recovery, auth hook; repo name cache; domain error → gRPC code mapping) and `xylemd server` (--listen, --http-listen, --object-cache-bytes; /healthz, /readyz = Postgres ping + store probe, /metrics; SIGTERM drains 10 s); ListRefs implemented early (exercises the stack); prometheus client_golang v1.24.1
- 2026-10-14 M3: `make reset-db` now runs `xylemd migrate` (was a stub)
- 2026-10-14 M3.8: tests — tiny/edge/branchy import + verify-import --all (0 mismatches); kill-and-resume now ends with verify-import --all; incremental import (edits, delete, symlink, exec, merged feature branch) creates only the new commits and verify-import --since checks just those; CLI verify-import (--all, --sample/--seed, --since, mismatch exit). Added VerifyOptions.Since / --since
- 2026-10-14 M3.7: importer.VerifyImport + `xylemd verify-import --git (--all | --sample N --seed S)`: ls-tree vs in-process tree walk (paths, modes, sizes, content via git_blobs or re-hash); branchy --all compares 2,000 commits / 567k file entries with 0 mismatches; detects a wrong blob mapping, a wrong root tree and a partial import
- 2026-10-14 M3.6: `xylemd import --git --branch --repo --follow --batch --limit --workers` with a progress line every 5 s (commits/s, blobs/s, bytes) and a summary; --follow polls until interrupted; e2e: branchy via the binary 2,000 commits in 3.3 s, no-op re-run 20 ms, verify clean; command test covers re-import and --follow picking up a new commit
- 2026-10-14 M3.5: checkpoints every --batch (git_blobs rows, import_state, main fast-forwarded to the newest imported commit on the branch's first-parent chain) and on exit incl. cancellation; tests: limited import checkpoints + main moves only along the fp chain, kill-and-resume (cancel at ≥400 commits, re-run → exactly one changeset per commit, verify clean), stale import_state; fixed a data race (producer read the sha map while the committer wrote it) found by -race
- 2026-10-14 M3.4: internal/importer pipeline — producer (rev-list, skip mapped), N workers (ReadCommit, DiffTree, blob upload via LRU → git_blobs → cat-file+Put), ordered committer (CreateChangeset AllowRoot/AllowEmpty/explicit committer/TrustSizes); tiny/edge/branchy import with tree-for-tree match vs git ls-tree and verify clean; branchy ~595 commits/s
- 2026-10-14 M3.3: importer.MapCommit (modes, A/M/T→PUT, D→DELETE, submodules skipped + extra, git-sha/git-encoding extras, exact signatures, `.xylem` paths → ErrUnsupportedPath) and BlobsNeeded; every edge-fixture commit maps to a shape-valid changeset; 97.5% coverage
- 2026-10-14 M3.2: internal/gitsource — Open (sha1 check), ResolveBranch, RevList (streamed iter.Seq2), CatFilePool (N long-lived cat-file --batch, lazy start, process replaced on stream errors), ReadCommit/ParseCommit, DiffTree (-z --raw --no-renames), LsTree; tests compare against git output on every tiny/edge commit; 80% coverage
- 2026-10-14 M3.1: scripts/make-fixture-repo.sh (tiny: 10 commits; edge: 27 commits, 2 roots, octopus, side-first merge, file↔symlink↔dir, exec flips, odd paths, 11 MiB binary, empty commit, ISO-8859-1 header, submodule add/move/remove) and scripts/genrepo (branchy: 2,000 commits, 81 merges via fast-import in ~1.6 s); all deterministic and pinned by sha; `make fixtures`; testutil.GitFixture/CopyFixture/Git
- 2026-10-14 M2.9: TestStress — 10k changesets, random puts/deletes, ~5% merges via a side branch, main fast-forwarded in steps, full Verify with objects → 0 violations
- 2026-10-14 M2.8: repo.Verify + `xylemd verify <repo> [--all|--sample N] [--objects]` (prints violations, exits non-zero); 13 tampering tests each detected (root tree, gen, jump, body, columns, file rows, seq, mainline gap/wrong/extra, dangling ref, missing blob, missing tree)
- 2026-10-14 M2.7: repo.UpdateRef (ref-name check, fast-forward via IsFirstParentAncestor, mainline rows from fp walk, forced rewind via binary-searched agreement depth, CAS errors → ErrConflict), repo.Init, ResolveHex; `xylemd admin init-repo` and `admin set-ref [--force] [--expected]` (--db, --store); tests check mainline == fp chain after every update
- 2026-10-14 M2.6: internal/repo — Open, CreateChangeset (07 steps 1–11: shape, parents, blob existence + header-size check, committer, encode, idempotency, Apply + store trees, ComputeCore, one tx), MissingBlobsError, domain errors; repotest.New/Put/Commit; 10 concurrent identical creates → 1 created + 9 already_existed
- 2026-10-14 M2.5: internal/derived core — ComputeCore (gen, fp_depth, fp_parent, skew-binary jump), CachedSource (LRU over meta), AncestorAtDepth/IsFirstParentAncestor/IsAncestor/MergeBases; property tests vs naive on random DAGs (≤300 nodes, 199 criss-cross pairs) and a 10k-deep tree with an O(log) read bound; mutation-checked
- 2026-10-14 M2.4: internal/treeops — Trees (LRU of decoded trees, golang-lru v2.0.7), Lookup, Apply (deletes+prune, puts, bottom-up encode; ErrDeleteMissing/ErrNoopPut/FileDirClashError/ErrDuplicatePath); property test: 1748 valid lists equal from-scratch builds, 2002 invalid rejected; mutation-checked
- 2026-10-14 M2.3: internal/meta Store + PG — repos (cached), InsertChangeset (one tx, ON CONFLICT, COPY > 1000 files), GetChangesets/GetCore/ResolvePrefix/GetByGitSHA/ChangesetFiles, GetRef/CreateRef/CASRef (mainline + ref_log in the same tx), MainlineLookup/MainlineAt, PathHistoryMainline (04 query); 13 integration tests incl. 10-way concurrent insert and CAS
- 2026-10-14 M2.2: testutil.NewDB (template xylem_tmpl_<fingerprint> under advisory lock, per-test clone, dropped on cleanup); 20 parallel clones ≈ 0.1–0.23 s each
- 2026-10-14 M2.1: migrations/0001_init.sql (verbatim from 03, no bugs found), meta.LoadMigrations/Migrate/Fingerprint (one tx per file, advisory lock), `xylemd migrate` (--db / XYLEM_DB_URL, --log-level); testutil.NewEmptyDB; pgx v5.11.0
- 2026-10-14 M1.9: FuzzDecodeTree, FuzzDecodeChangeset (byte-identical re-encode), FuzzObjectDecode (content-level re-encode + Verify, since zstd frames are not canonical), all seeded from golden vectors; picked up by make fuzz
- 2026-10-14 M1.8: golden vectors **approved by the human**; encodings frozen (changes now need an ADR)
- 2026-10-14 M1.8: golden vectors — 15 vectors (2 blobs, 5 trees, 8 changesets) in testdata/golden with generated, annotated README; test/golden checks bytes+ids (UPDATE_GOLDEN=1 regenerates); all ids cross-checked with Python blake3 and bodies parsed by an independent spec-based Python decoder; awaiting human approval
- 2026-10-14 M1.7: internal/store/storetest contract suite (put/get, not found, has incl. order+dups, idempotent put, 32 concurrent same-id puts, 64 distinct, 24 MiB object (S3 multipart), canceled ctx); passes for mem, fs and s3 (-tags integration, MinIO); mutation-checked; store coverage 88.7% with integration
- 2026-10-14 M1.6: internal/store — ObjectStore, ErrNotFound, Key, Open(mem/fs/s3 URL, secrets redacted in errors); Mem (copying), FS (temp+fsync+rename, skip if present), S3 (minio-go v7.3.0, Has via StatObject x32 via x/sync v0.24.0 errgroup); S3 smoke-checked against local MinIO; full contract suite is M1.7
- 2026-10-14 M1.5: internal/changeset — Changeset/Signature/Change, Encode (sorts copies; rejects dup paths and op/mode mismatches), ID, Decode (canonical only), ValidateShape(ShapeOptions{AllowEmpty}), Summary; internal/canon extracted; 99.4% coverage, hand-computed layout test
- 2026-10-14 M1.4: internal/object tree encoding — Mode, Entry, Tree, EncodeTree (validates order/names/modes, rejects size on TREE), DecodeTree (canonical only: minimal varints, no trailing bytes, bounded prealloc), Lookup; hand-computed layout test; backfilled M1.3 mutation property; 98.7% coverage
- 2026-10-14 M1.3: internal/object framing — Kind, Header, ID, Encode/Decode/DecodeVerify/Verify; shared zstd level-3 encoder/decoder (concurrent-safe, internally pooled), 64 MiB + 1 KiB decode cap (bomb-tested incl. streamed frames), strict canonical header parsing; klauspost/compress v1.20.1; 96.7% coverage
- 2026-10-14 M1.2: internal/paths — ValidName/ValidPath (rule-specific errors), Split/Join/Parent/Base, IsUnder (dir itself counts; root = empty), Compare, DirPrefixRange (strictly below dir, nil = open); 100% coverage; FuzzValidPath 30 s clean
- 2026-10-14 M1.1: internal/hash — ID, Sum, String/Short/IsZero, Compare, ParseHex, ParsePrefix (lo/hi byte range); blake3 v0.2.4, go-cmp v0.7.0; 100% coverage, BLAKE3 official vectors
- 2026-10-14 M0.8: cmd/xylemd/main_test.go (version output, link-time and fallback; rejects args), README Getting started
- 2026-10-14 M0.7: .github/workflows/ci.yml — lint, unit, integration (postgres service + MinIO step + mc mb step), build, proto freshness; actionlint clean, MinIO steps simulated locally; not yet pushed
- 2026-10-14 M0.6: buf.yaml v2 (STANDARD/FILE) + buf.gen.yaml (local plugins); 4 protos copied verbatim from 05-api.md; gen/xylem/v1 committed; `make check` green
- 2026-10-14 M0.5: .golangci.yml (v2; 8 linters, gofumpt+goimports, gen/ excluded — verified with a probe), .gitignore, .editorconfig, MIT LICENSE
- 2026-10-14 M0.4: deploy/docker-compose.yml (postgres:17.11, pgsty/minio + pgsty/mc per ADR-020); `make up` verified fresh, rerun and cold
- 2026-10-14 M0.3: Makefile + tools.mk (buf v1.73.0, golangci-lint v2.14.0, grpcurl v1.9.4, protoc-gen-go v1.36.12, protoc-gen-go-grpc v1.6.2); later-milestone targets stubbed; `buf lint` green from M0.6
- 2026-10-14 M0.2: internal/* skeleton (19 pkgs), cmd/xy + cmd/xylemd cobra roots with `version`
- 2026-10-14 M0.1: module `github.com/delefrati/xylem` (go 1.27.2); OWNER replaced in docs
