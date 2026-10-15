# 05 — API (gRPC)

The `.proto` files under `proto/xylem/v1/` are the source of truth once created. This document is their initial content. Generated Go code goes to `gen/xylem/v1` (package `xylemv1`) and is committed (ADR-014).

## Conventions

- Every request carries `string repo` (MVP default: `"default"`).
- Ids are `bytes` (32 raw bytes) on the wire. Clients render them as hex.
- Paths are `bytes`.
- Lists are paginated with `page_size` (default 100, max 1000) and an opaque `page_token`.
- Batches are capped at 1024 ids per `GetObjects`/`HasObjects` call. The server returns `InvalidArgument` above that.
- Large results are server-streamed.
- **Message size:** set `grpc.MaxRecvMsgSize` and `grpc.MaxSendMsgSize` to 80 MiB on every server, tier and client (default is 4 MiB, smaller than the 64 MiB max blob). Senders pack multiple objects per message up to ~1 MiB; a single object larger than that is sent alone in its own message.

## Error model

| gRPC code | When |
|---|---|
| `InvalidArgument` | Malformed id, path or revision; ambiguous prefix; validation failure (the message says which rule) |
| `NotFound` | Unknown repo, revision, path or object |
| `FailedPrecondition` | Binary file for blame; missing blobs on `CreateChangeset` (the message lists up to 20 hex ids) |
| `Aborted` | Ref CAS mismatch on `UpdateRef` (the client may retry) |
| `ResourceExhausted` | Query cost limit hit, or rate limit (M9) |
| `Unauthenticated` | Missing or invalid token (M9.1) |
| `PermissionDenied` | Valid token without the role the RPC needs (M9.1) |

Merge conflicts are **not** errors. `Land` and `Rebase` return them in the response body.

## `proto/xylem/v1/common.proto`

```proto
syntax = "proto3";
package xylem.v1;
option go_package = "github.com/delefrati/xylem/gen/xylem/v1;xylemv1";

import "google/protobuf/timestamp.proto";

enum FileMode {
  FILE_MODE_UNSPECIFIED = 0;
  FILE_MODE_FILE = 1;
  FILE_MODE_EXEC = 2;
  FILE_MODE_SYMLINK = 3;
  FILE_MODE_TREE = 4;
}

message Signature {
  bytes name_email = 1;                        // "Name <email>"; bytes because git allows non-UTF-8
  google.protobuf.Timestamp time = 2;          // only seconds are significant; nanos must be 0
  sint32 tz_offset_minutes = 3;
}

message FileChange {
  bytes path = 1;
  enum Op { OP_UNSPECIFIED = 0; OP_PUT = 1; OP_DELETE = 2; }
  Op op = 2;
  FileMode mode = 3;                           // PUT only
  uint64 size = 4;                             // PUT only
  bytes blob_id = 5;                           // PUT only
}

message Changeset {
  bytes id = 1;
  uint64 seq = 2;
  repeated bytes parents = 3;
  Signature author = 4;
  Signature committer = 5;
  bytes message = 6;                           // raw bytes (may be non-UTF-8 for imported history)
  map<string, bytes> extras = 7;
  bytes root_tree = 8;                         // from core_derived
  uint64 generation = 9;
  uint64 first_parent_depth = 10;
  bool on_mainline = 11;
  string git_sha = 12;                         // hex, empty if native
  uint32 change_count = 13;
  repeated FileChange changes = 14;            // only when requested
}

message PageRequest { uint32 page_size = 1; string page_token = 2; }
```

## `proto/xylem/v1/object.proto`

```proto
syntax = "proto3";
package xylem.v1;
option go_package = "github.com/delefrati/xylem/gen/xylem/v1;xylemv1";

// Implemented identically by server, cache and daemon tiers.
service ObjectService {
  // Streams objects in stored format (zstd frame of header+body), in any order.
  rpc GetObjects(GetObjectsRequest) returns (stream GetObjectsResponse);
  rpc HasObjects(HasObjectsRequest) returns (HasObjectsResponse);
  // Server tier only; cache and daemon tiers return Unimplemented.
  rpc PutObjects(stream PutObjectsRequest) returns (PutObjectsResponse);
}

message StoredObject {
  bytes id = 1;
  bytes data = 2;                              // zstd frame; receiver verifies the hash
}

message GetObjectsRequest { string repo = 1; repeated bytes ids = 2; }
message GetObjectsResponse {
  repeated StoredObject objects = 1;           // packed up to ~1 MiB per message; a larger object goes alone
  repeated bytes missing = 2;                  // ids not found (sent in the final message)
}

message HasObjectsRequest { string repo = 1; repeated bytes ids = 2; }
message HasObjectsResponse { repeated bool present = 1; }   // same order as request

message PutObjectsRequest { string repo = 1; repeated StoredObject objects = 2; }
message PutObjectsResponse { uint32 stored = 1; uint32 already_present = 2; }
```

## `proto/xylem/v1/repo.proto`

```proto
syntax = "proto3";
package xylem.v1;
option go_package = "github.com/delefrati/xylem/gen/xylem/v1;xylemv1";

import "xylem/v1/common.proto";

service RepoService {
  rpc ResolveRevision(ResolveRevisionRequest) returns (ResolveRevisionResponse);
  rpc GetChangesets(GetChangesetsRequest) returns (GetChangesetsResponse);
  rpc ListRefs(ListRefsRequest) returns (ListRefsResponse);
  rpc UpdateRef(UpdateRefRequest) returns (UpdateRefResponse);          // admin; land uses WriteService

  rpc Log(LogRequest) returns (LogResponse);
  rpc ListDirectory(ListDirectoryRequest) returns (ListDirectoryResponse);
  rpc ReadFile(ReadFileRequest) returns (stream ReadFileResponse);
  rpc Diff(DiffRequest) returns (stream DiffResponse);
  rpc Blame(BlameRequest) returns (BlameResponse);

  rpc IsAncestor(IsAncestorRequest) returns (IsAncestorResponse);
  rpc MergeBase(MergeBaseRequest) returns (MergeBaseResponse);
}

message ResolveRevisionRequest { string repo = 1; string revision = 2; }
message ResolveRevisionResponse { bytes id = 1; bytes root_tree = 2; }

message GetChangesetsRequest {
  string repo = 1;
  repeated bytes ids = 2;                      // max 1000
  bool include_changes = 3;
}
message GetChangesetsResponse { repeated Changeset changesets = 1; }

message Ref { string name = 1; bytes id = 2; uint64 version = 3; }
message ListRefsRequest { string repo = 1; string prefix = 2; }
message ListRefsResponse { repeated Ref refs = 1; }

message UpdateRefRequest {
  string repo = 1;
  string name = 2;
  bytes expected_id = 3;                       // empty = must not exist
  bytes new_id = 4;
  bool force = 5;                              // allow non-fast-forward (admin)
}
message UpdateRefResponse { Ref ref = 1; }

message LogRequest {
  string repo = 1;
  string revision = 2;                         // default "main"
  bytes path = 3;                              // empty = whole repo
  PageRequest page = 4;
}
message LogResponse { repeated Changeset changesets = 1; string next_page_token = 2; }

message DirEntry { bytes name = 1; FileMode mode = 2; bytes id = 3; uint64 size = 4; }
message ListDirectoryRequest {
  string repo = 1;
  string revision = 2;
  bytes path = 3;                              // empty = root
  bool recursive = 4;                          // recursive returns full paths in name
  PageRequest page = 5;
}
message ListDirectoryResponse {
  bytes tree_id = 1;
  repeated DirEntry entries = 2;
  string next_page_token = 3;
}

message ReadFileRequest { string repo = 1; string revision = 2; bytes path = 3; }
message ReadFileResponse {
  FileMode mode = 1;                           // first message only
  uint64 size = 2;                             // first message only
  bytes blob_id = 3;                           // first message only
  bytes chunk = 4;                             // ≤ 1 MiB per message
}

message DiffRequest {
  string repo = 1;
  string from_revision = 2;                    // empty = first parent of to_revision
  string to_revision = 3;
  repeated bytes paths = 4;                    // optional prefix filter
  bool include_hunks = 5;
  uint32 context_lines = 6;                    // default 3
}
message DiffHunk {
  uint32 old_start = 1; uint32 old_lines = 2;
  uint32 new_start = 3; uint32 new_lines = 4;
  bytes body = 5;                              // unified diff lines with ' ', '-', '+' prefixes
}
message FileDiff {
  bytes path = 1;
  enum Status { STATUS_UNSPECIFIED = 0; STATUS_ADDED = 1; STATUS_DELETED = 2; STATUS_MODIFIED = 3; STATUS_MODE_CHANGED = 4; }
  Status status = 2;
  FileMode old_mode = 3; FileMode new_mode = 4;
  bytes old_blob = 5; bytes new_blob = 6;
  bool binary = 7;
  repeated DiffHunk hunks = 8;
}
message DiffResponse { repeated FileDiff files = 1; }

message BlameRequest { string repo = 1; string revision = 2; bytes path = 3; }
message BlameLine {
  bytes origin = 1;                            // changeset id
  uint32 origin_line = 2;                      // 1-based
  bytes content = 3;                           // line without "\n"
}
message BlameResponse {
  repeated BlameLine lines = 1;
  repeated Changeset origins = 2;              // metadata for distinct origins (no changes)
}

message IsAncestorRequest { string repo = 1; string ancestor = 2; string descendant = 3; }
message IsAncestorResponse { bool result = 1; }

message MergeBaseRequest { string repo = 1; string a = 2; string b = 3; }
message MergeBaseResponse { repeated bytes ids = 1; }
```

## `proto/xylem/v1/write.proto`

```proto
syntax = "proto3";
package xylem.v1;
option go_package = "github.com/delefrati/xylem/gen/xylem/v1;xylemv1";

import "xylem/v1/common.proto";

service WriteService {
  rpc CreateChangeset(CreateChangesetRequest) returns (CreateChangesetResponse);
  rpc Rebase(RebaseRequest) returns (RebaseResponse);
  rpc Land(LandRequest) returns (LandResponse);
}

message CreateChangesetRequest {
  string repo = 1;
  repeated bytes parents = 2;
  Signature author = 3;                        // server fills committer
  bytes message = 4;
  map<string, bytes> extras = 5;
  repeated FileChange changes = 6;             // any order; server canonicalizes
}
message CreateChangesetResponse { Changeset changeset = 1; bool already_existed = 2; }

message Conflict {
  bytes path = 1;
  enum Kind {
    KIND_UNSPECIFIED = 0;
    KIND_CONTENT = 1;                               // both sides changed the file differently
    KIND_DELETE_MODIFY = 2;
    KIND_ADD_ADD = 3;
    KIND_FILE_DIRECTORY = 4;
    KIND_MODE = 5;
  }
  Kind kind = 2;
  bytes base_blob = 3; bytes ours_blob = 4; bytes theirs_blob = 5;   // ours = onto side
  uint32 region_count = 6;                     // conflicting line regions (M7); 0 = whole-file conflict
}

message RebaseRequest {
  string repo = 1;
  repeated bytes stack = 2;                    // bottom → top; each's first parent is the previous one
  string onto = 3;                             // revision
}
message RebaseResponse {
  repeated bytes rebased = 1;                  // rebased versions of the non-skipped entries, stack order; empty on conflict
  repeated Conflict conflicts = 2;
  bytes conflicting_changeset = 3;             // the stack entry that conflicted
  repeated bytes skipped = 4;                  // original ids whose changes were all already present on onto
}

message LandRequest {
  string repo = 1;
  repeated bytes stack = 2;                    // bottom → top
  string target_ref = 3;                       // default "main"
  uint32 max_attempts = 4;                     // default 10
}
message LandResponse {
  repeated bytes landed = 1;                   // ids now on target (rebased versions)
  bytes new_tip = 2;
  repeated Conflict conflicts = 3;             // non-empty means nothing landed
  bytes conflicting_changeset = 4;
  uint32 attempts = 5;
  repeated bytes skipped = 6;                  // original ids that turned out to be no-ops (already on target)
}
```

## Server implementation notes

- **Interceptors (M4):** request id, structured logging (method, repo, duration, code), Prometheus metrics (`xylem_rpc_duration_seconds{method,code}`), panic recovery, and bearer-token auth (M9.1, below).
- **Auth (M9.1):** with `--auth-tokens`, every call needs `authorization: Bearer <token>`. The tokens file maps each token to an identity (`"Name <email>"`) and roles. Read RPCs (RepoService queries, `GetObjects`, `HasObjects`, reflection) need `read`; `UpdateRef`, `PutObjects` and every WriteService RPC need `write`, which implies `read`. RPCs missing from the role table (`internal/auth`) are denied. The identity is the committer of changesets created by `CreateChangeset`, `Rebase` and `Land`, and the actor in the ref log. Tokens are compared as SHA-256 digests in constant time.
- **Cost limits:** Log and Diff stop after scanning 100k rows or tree entries, or 10 s per page, and return a `next_page_token` (or `ResourceExhausted` if no progress was possible).
- **Health:** `/healthz` (process up) and `/readyz` (Postgres ping plus object store `Has` on a sentinel object).
- Enable gRPC server reflection in dev builds so `grpcurl` works.
