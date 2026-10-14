-- 0001_init: initial schema, verbatim from docs/03-data-model.md §Postgres schema.

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
