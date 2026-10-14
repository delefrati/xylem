-- 0002_git_blobs: git blob sha → Xylem blob id, filled by the importer (docs/08-git-import.md §Blob mapping table).

CREATE TABLE git_blobs (
  repo_id   SMALLINT NOT NULL REFERENCES repos(id),
  git_sha   BYTEA    NOT NULL CHECK (octet_length(git_sha) = 20),
  blob_id   BYTEA    NOT NULL CHECK (octet_length(blob_id) = 32),
  size      BIGINT   NOT NULL,
  PRIMARY KEY (repo_id, git_sha)
);
