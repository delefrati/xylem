# M1 — Core objects and stores

**Goal:** hash, path, blob, tree and changeset encodings that are exactly as specified and frozen by golden vectors, plus the object store implementations.
**Depends on:** M0.
**Read first:** `03-data-model.md` (all of it), `12-decisions.md` (ADR-003, 004, 005, 006, 015, 017), `10-testing.md` §Golden vectors and §Property tests.

## Tasks

- [x] **M1.1 `internal/hash`.** `type ID [32]byte`, `Sum(header, body)`, `String()` (hex), `ParseHex`, `ParsePrefix` (≥ 8 hex characters → inclusive lower bound and exclusive upper bound for range queries; an odd-length prefix is padded with `0` for the lower bound and treated as covering all 16 values of the last nibble; the upper bound is open (nil) when the prefix is all `f`), `Compare`, `IsZero`, and a `Short(n)` helper. Use `github.com/zeebo/blake3`. Unit tests.
- [x] **M1.2 `internal/paths`.** `ValidName`, `ValidPath`, `Split`, `Join`, `Parent`, `Base`, `IsUnder(path, dir)`, `Compare` (bytewise), and `DirPrefixRange(dir) (lo, hi []byte)` for SQL range queries. Reject `.xylem` and NUL. Fuzz `ValidPath` against `Split`/`Join` round trip.
- [ ] **M1.3 `internal/object` framing.** `Kind` constants, `Header(kind, n)`, `Encode(kind, body) (ID, stored []byte)`, `Decode(stored) (Kind, body, error)`, `Verify(id, stored) error`. zstd level 3 with pooled encoders and decoders (`klauspost/compress/zstd`). Enforce a max decoded size (default 64 MiB + 1 KiB) to prevent decompression bombs.
- [ ] **M1.4 Tree encoding.** `Mode` (u8 values from 03), `Entry{Mode, Name, ID, Size}`, `Tree{Entries}`. `EncodeTree` validates strict ordering, unique names and valid names; `DecodeTree` rejects non-canonical input (unsorted entries, duplicates, trailing bytes, non-minimal varints). Empty trees are encodable; whether one is allowed (root only) is checked by `treeops` in M2. Add `(*Tree).Lookup(name)` via binary search.
- [ ] **M1.5 `internal/changeset`.** `Changeset` struct mirroring the canonical body. `Encode()` (canonical, sorts changes and extras), `Decode()` (rejects non-canonical input), `ID()`, `ValidateShape()` (03 §Validation rules 2 and 5, plus op/mode consistency). Add a `Summary()` helper (first line, ≤ 200 runes).
- [ ] **M1.6 `internal/store`.** `ObjectStore` interface (03). Implementations: `Mem` (map + RWMutex), `FS` (sharded `objects/ab/cd/<hex>`, write via temp file + `fsync` + rename, `Has` via stat), `S3` (minio-go; `Has` via `StatObject` with bounded concurrency). Add `store.Open(url)` that parses `mem://`, `fs://path` and `s3://bucket?...`.
- [ ] **M1.7 Store contract tests.** A shared `storetest.Run(t, factory)` suite (put, get, has, idempotent put, not found, concurrent puts of the same id, large object). Run it for Mem and FS in unit tests, and for S3 under `-tags integration` against MinIO.
- [ ] **M1.8 Golden vectors ⛔.** Generate `testdata/golden/` for the cases listed in `10-testing.md`. Add a test that re-encodes the fixtures and compares bytes and ids, with an `UPDATE_GOLDEN=1` regeneration flag. Write a human-readable `testdata/golden/README.md` listing each vector's inputs, hex encoding and id. **Stop and ask the human to review before continuing.**
- [ ] **M1.9 Fuzz targets.** `FuzzDecodeTree`, `FuzzDecodeChangeset`, `FuzzObjectDecode` (must never panic; any successfully decoded input must re-encode to identical bytes). Wire them into `make fuzz`.

## Acceptance criteria

- Golden vectors approved by the human (record the approval date in `progress.md`).
- `make fuzz` runs 30 s per target with no crashes.
- Store contract suite passes for mem, fs and s3.
- Coverage of `hash`, `paths`, `object` and `changeset` is ≥ 90% (`go test -cover`).

## Verification

```bash
make test && make test-integration && make fuzz
go test -cover ./internal/hash/... ./internal/paths/... ./internal/object/... ./internal/changeset/...
```

## Out of scope

Postgres, trees-by-path operations (M2), any RPC.
