# 03 — Data model

> **Hash-affecting rules.** Everything in "Canonical encodings" determines object and changeset ids. Changing it breaks every stored id. An implementing agent must **not** change these encodings without explicit human approval, recorded as an ADR.

## Identifiers

- `ObjectID` is 32 bytes: BLAKE3-256 output.
- Display form is 64 lowercase hex characters. The CLI accepts unique prefixes of at least 8 hex characters.
- The same `ObjectID` type is used for blobs, trees, changesets and stored derived values. Kinds are domain-separated by the header (below), so ids never collide across kinds.

### Hashing

```
id = BLAKE3-256( header || body )
header = ASCII "<kind> <decimal body length>" followed by one 0x00 byte
kind  ∈ { "blob", "tree", "changeset", "derived" }
```

Example: an empty blob has header `"blob 0\x00"` and an empty body.

## Primitive encodings (used by tree and changeset bodies)

| Name | Encoding |
|---|---|
| `uvarint` | Go `binary.AppendUvarint` (LEB128, unsigned) |
| `varint` | Go `binary.AppendVarint` (zig-zag) |
| `bytes` | `uvarint(len) ‖ raw bytes` |
| `id` | 32 raw bytes |
| `u8` | one byte |

## Canonical encodings

### Paths and names

- A **name** is a non-empty byte string with no `/` and no `0x00`, and is not `.`, `..` or `.xylem`. It is *not* required to be UTF-8 (git allows arbitrary bytes). Paths are stored as raw bytes everywhere (ADR-015).
- A **path** is names joined by `/`: no leading or trailing `/`, no empty components.
- **Ordering** is plain bytewise comparison (`bytes.Compare`) of full paths or names. Note this differs from git's tree ordering, which sorts directories as if suffixed with `/`. Xylem doesn't need git-compatible tree hashes.

### File modes

| Mode | u8 | git equivalent |
|---|---|---|
| `FILE` | `0x01` | 100644 |
| `EXEC` | `0x02` | 100755 |
| `SYMLINK` | `0x03` | 120000 (blob holds the link target) |
| `TREE` | `0x04` | 040000 |

Git submodules (`160000`) are skipped on import with a warning.

### Blob

`body` = the raw file bytes, unmodified (no EOL conversion).

### Tree

```
body = uvarint(entry_count) ‖ entry*        // entries strictly ascending by name, unique
entry = u8(mode) ‖ bytes(name) ‖ id ‖ [ uvarint(size) if mode != TREE ]
```

- `size` is the referenced blob's byte length. It is included so a future virtual filesystem can `stat` without fetching blobs (ADR-017).
- An empty tree (`entry_count = 0`) is legal **only** as a root tree (an empty repository). Directories with no files don't exist.

### Changeset

```
body =
  uvarint(1)                                  // format version
  uvarint(parent_count) ‖ id*                 // order matters: [0] is the first parent
  bytes(author)            varint(author_time_unix)    varint(author_tz_minutes)
  bytes(committer)         varint(committer_time_unix) varint(committer_tz_minutes)
  bytes(message)
  uvarint(extra_count)  ‖ (bytes(key) ‖ bytes(value))*  // ascending by key, unique keys
  uvarint(change_count) ‖ change*                         // ascending by path, unique paths
change =
  bytes(path) ‖ u8(op)                        // op: 0x01 PUT, 0x02 DELETE
  [ if PUT: u8(mode) ‖ uvarint(size) ‖ id(blob) ]          // mode ∈ FILE|EXEC|SYMLINK
```

- `author` and `committer` are `"Name <email>"` as raw bytes (git allows non-UTF-8 identities and messages; everything that carries them is `bytes`/`BYTEA`, never a Go `string` sent through proto `string` fields).
- Times are whole Unix seconds (sub-second precision is dropped everywhere, including proto `Timestamp.nanos`, which must be 0). `tz_minutes` is the signed UTC offset, e.g. `-180` for `-0300`.
- Standard extras: `git-sha` (40 hex, set by the importer), `rebased-from` (64 hex, set by land and rebase), `git-extra-parents-unmapped` (importer edge cases). Unknown extras are preserved.
- **Changes are relative to the first parent.** For a root changeset (no parents) they are relative to the empty tree. For merges, changes from parents other than the first are *not* recorded separately; the changes list is simply `tree(self) − tree(parents[0])`.
- The root tree id is **not** part of the changeset. It is derived data (ADR-006).

### Validation rules for a changeset (enforced by `changeset.Validate` and the server)

1. All parents exist. `parent_count ≤ 16`. Root changesets are only allowed when the repo is empty or with an explicit `allow_root` flag (the importer uses it).
2. Paths are valid and `change_count ≥ 1`, except for merges and the empty-root case.
3. Every PUT blob exists in the object store, and its declared size matches the blob. Mechanism: one store read per *newly referenced* blob (a blob whose id differs from the parent's entry at that path), decompressing only until the header `blob <len>\0` has been parsed (streaming zstd decoder; stop early). The importer, which computed the sizes itself, may skip this check (`TrustSizes` option).
4. Applying the changes to the first parent's tree yields a valid tree. Deletes apply first, then puts. A path must not be both a file and a directory prefix in the result. Deleting a path that doesn't exist in the parent is an error. A PUT identical to the parent entry (same mode and blob) is an error ("no-op change"), so changes lists stay minimal.
5. `message` ≤ 1 MiB, each extra value ≤ 64 KiB, `change_count ≤ 1,000,000`.

## Stored object format (identical at rest and on the wire)

```
stored = zstd_frame( header || body )
```

- One zstd frame per object, default level 3.
- Readers verify `BLAKE3(decompressed) == id` on every read from an untrusted tier. Servers verify on `PutObjects`.
- Blobs larger than 64 MiB are rejected in the MVP (configurable). Chunking is future work.

### Object store key layout (fs and S3)

```
objects/<first 2 hex>/<next 2 hex>/<full 64 hex>
```

The store doesn't care about kind; the kind is in the header.

```go
// internal/store
type ObjectStore interface {
    Get(ctx context.Context, id hash.ID) ([]byte, error)          // returns the stored zstd frame; ErrNotFound
    Put(ctx context.Context, id hash.ID, stored []byte) error      // idempotent
    Has(ctx context.Context, ids []hash.ID) ([]bool, error)
}
```

`internal/object` provides `Encode(kind, body) (id, stored)`, `Decode(stored) (kind, body, error)` and `Verify(id, stored)`.

## Postgres schema (`migrations/0001_init.sql`)

All ids are `BYTEA` of length 32 (enforced by `CHECK (octet_length(x) = 32)`). Paths are `BYTEA`.

```sql
CREATE TABLE repos (
  id          SMALLINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The write-ahead log of history. body is the canonical encoding (source of truth);
-- the other columns are denormalized for queries.
CREATE TABLE changesets (
  repo_id         SMALLINT    NOT NULL REFERENCES repos(id),
  id              BYTEA       NOT NULL CHECK (octet_length(id) = 32),
  seq             BIGINT      GENERATED ALWAYS AS IDENTITY,
  parents         BYTEA[]     NOT NULL,
  body            BYTEA       NOT NULL,
  author          BYTEA       NOT NULL,          -- raw "Name <email>" (may be non-UTF-8)
  author_time     TIMESTAMPTZ NOT NULL,
  committer       BYTEA       NOT NULL,
  committer_time  TIMESTAMPTZ NOT NULL,
  summary         TEXT        NOT NULL,          -- strings.ToValidUTF8(first line, "\uFFFD"), ≤ 200 chars
  git_sha         BYTEA       CHECK (git_sha IS NULL OR octet_length(git_sha) = 20),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, id)
);
CREATE UNIQUE INDEX changesets_seq_idx ON changesets (repo_id, seq);
CREATE UNIQUE INDEX changesets_git_idx ON changesets (repo_id, git_sha) WHERE git_sha IS NOT NULL;

-- One row per changed path (relative to the first parent). Also the path-history index.
CREATE TABLE changeset_files (
  repo_id       SMALLINT NOT NULL,
  changeset_id  BYTEA    NOT NULL,
  seq           BIGINT   NOT NULL,                 -- copied from changesets.seq
  path          BYTEA    NOT NULL,
  op            SMALLINT NOT NULL,                 -- 1 PUT, 2 DELETE
  mode          SMALLINT,                          -- NULL for DELETE
  size          BIGINT,
  blob_id       BYTEA,
  PRIMARY KEY (repo_id, changeset_id, path),
  FOREIGN KEY (repo_id, changeset_id) REFERENCES changesets (repo_id, id)
);
CREATE INDEX changeset_files_path_idx ON changeset_files (repo_id, path, seq DESC);

-- Eager derived data computed in the same transaction as the changeset insert.
CREATE TABLE core_derived (
  repo_id       SMALLINT NOT NULL,
  changeset_id  BYTEA    NOT NULL,
  root_tree     BYTEA    NOT NULL,
  gen           BIGINT   NOT NULL,                 -- 1 + max(gen(parents)); roots = 1
  fp_depth      BIGINT   NOT NULL,                 -- first-parent depth; roots = 0
  fp_parent     BYTEA,                             -- NULL for roots
  fp_jump       BYTEA    NOT NULL,                 -- skew-binary jump pointer (see 04)
  PRIMARY KEY (repo_id, changeset_id),
  FOREIGN KEY (repo_id, changeset_id) REFERENCES changesets (repo_id, id)
);

CREATE TABLE refs (
  repo_id       SMALLINT    NOT NULL REFERENCES repos(id),
  name          TEXT        NOT NULL,              -- "main", "release/2026.10"
  changeset_id  BYTEA       NOT NULL,
  version       BIGINT      NOT NULL DEFAULT 1,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, name)
);

CREATE TABLE ref_log (
  repo_id     SMALLINT    NOT NULL,
  name        TEXT        NOT NULL,
  version     BIGINT      NOT NULL,
  old_id      BYTEA,
  new_id      BYTEA       NOT NULL,
  actor       TEXT        NOT NULL,
  reason      TEXT        NOT NULL,                -- "land", "import", "admin"
  at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, name, version)
);

-- First-parent chain of selected refs (MVP: only "main"), indexed by depth.
CREATE TABLE mainline (
  repo_id       SMALLINT NOT NULL,
  ref_name      TEXT     NOT NULL,
  fp_depth      BIGINT   NOT NULL,
  changeset_id  BYTEA    NOT NULL,
  PRIMARY KEY (repo_id, ref_name, fp_depth)
);
CREATE UNIQUE INDEX mainline_cs_idx ON mainline (repo_id, ref_name, changeset_id);

-- Lazily computed derived values (blame etc.). Immutable once written.
CREATE TABLE derived (
  repo_id       SMALLINT NOT NULL,
  kind          TEXT     NOT NULL,
  kind_version  SMALLINT NOT NULL,
  changeset_id  BYTEA    NOT NULL,
  key           BYTEA    NOT NULL DEFAULT '',      -- e.g. path for blame
  value         BYTEA,                             -- inline if ≤ 4 KiB
  value_ref     BYTEA,                             -- object id ("derived" kind) otherwise
  computed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, kind, kind_version, changeset_id, key),
  CHECK ((value IS NULL) <> (value_ref IS NULL))
);

-- Importer bookkeeping.
CREATE TABLE import_state (
  repo_id        SMALLINT    NOT NULL REFERENCES repos(id),
  source         TEXT        NOT NULL,             -- e.g. file:///path/to/repo
  branch         TEXT        NOT NULL,
  last_git_sha   BYTEA,
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (repo_id, source, branch)
);
```

### Invariants

- `seq(child) > seq(parent)` always, because a changeset is only inserted after all its parents exist. Ordering by `seq DESC` among ancestors is therefore a valid reverse-topological order.
- `changesets`, `changeset_files`, `core_derived`, `derived` and objects are **immutable**. Only `refs`, `ref_log`, `mainline` and `import_state` change.
- A row in `changesets` implies its rows in `changeset_files` and `core_derived` exist (same transaction).
- `mainline` for ref R contains exactly the first-parent chain of `refs(R)`, depths 0..fp_depth(tip), updated in the same transaction as the ref CAS.

## Revision syntax (`internal/revision`)

| Syntax | Meaning |
|---|---|
| `<hex>` (≥ 8 chars) | changeset by unique id prefix (`id >= prefix AND id < prefix_upper` on the PK) |
| `main`, `release/x` | ref |
| `@` | workspace parent (client-side only) |
| `<rev>~N` | N-th first-parent ancestor (uses jump pointers) |
| `<rev>^N` | N-th parent (1-based) |
| `git:<sha or prefix>` | imported changeset by git sha |

Ambiguous prefixes return `InvalidArgument` listing up to 5 candidates.
