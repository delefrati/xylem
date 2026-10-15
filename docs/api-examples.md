# API examples (grpcurl)

Working `grpcurl` calls for every read RPC, against the `edge` fixture imported as the `default` repository. The outputs below are real, trimmed with `…`.

## Setup

```bash
make up && make build && make fixtures
bin/xylemd migrate
bin/xylemd import --git testdata/fixtures/edge --branch main
bin/xylemd server &                      # gRPC :7400, HTTP :7401
G="bin/grpcurl -plaintext"; A=localhost:7400
$G $A list                               # reflection: xylem.v1.RepoService, ObjectService, WriteService
```

## Conventions

- **Revisions are strings**: `main`, `main~2`, `main~1^2`, a hex id prefix (≥ 8 characters), or `git:<sha>` for imported commits (03 §Revision syntax).
- **`bytes` fields are base64** in grpcurl's JSON. That covers ids, paths, names, messages and file contents. Helpers:

  ```bash
  b64() { printf '%s' "$1" | base64; }             # path → base64:  b64 README  → UkVBRE1F
  unb64() { printf '%s' "$1" | base64 -d; echo; }   # base64 → bytes
  hex2b64() { printf '%s' "$1" | xxd -r -p | base64; }  # hex id → base64
  ```

- An empty `repo` means `default`. Read RPCs default `revision` to `main`.
- Changesets carry a readable `gitSha` when they were imported.

## RepoService

### ResolveRevision

```bash
$G -d '{"revision":"main~2"}' $A xylem.v1.RepoService/ResolveRevision
$G -d '{"revision":"git:1eae83c"}' $A xylem.v1.RepoService/ResolveRevision
```
```json
{
  "id": "3r+3JNt1/W55d+rbfO5Cw3Bp+CF1EPBLDK8q+SSeXdo=",
  "rootTree": "YtgLrRV1gtgQplFF5ml2PqdJwZxWvslI56S9c5846ug="
}
```

### ListRefs

```bash
$G -d '{}' $A xylem.v1.RepoService/ListRefs
$G -d '{"prefix":"release/"}' $A xylem.v1.RepoService/ListRefs
```
```json
{ "refs": [ { "name": "main", "id": "3r+3JNt1/W55d+rbfO5Cw3Bp+CF1EPBLDK8q+SSeXdo=", "version": "1" } ] }
```

### Log

The first-parent history of a revision, newest first. Pass `next_page_token` back for the next page. With `path` (base64), it only lists changesets that touch that file or directory, like `git log --first-parent -- <path>`.

```bash
$G -d '{"revision":"main","page":{"page_size":2}}' $A xylem.v1.RepoService/Log
$G -d "{\"path\":\"$(b64 x)\"}" $A xylem.v1.RepoService/Log
```
```json
{
  "changesets": [
    {
      "id": "3r+3JNt1/W55d+rbfO5Cw3Bp+CF1EPBLDK8q+SSeXdo=",
      "seq": "27",
      "parents": [ "I8K6CtCeoIWwRg1V+DN0womNIEHAvnqzRD9qlWlCXIM=" ],
      "author": { "nameEmail": "QWRhIExvdmVsYWNlIDxhZGFAZXhhbXBsZS5jb20+", "time": "2023-11-14T22:40:20Z" },
      "message": "dGFpbAo=",
      "generation": "21",
      "firstParentDepth": "20",
      "onMainline": true,
      "gitSha": "1eae83c0bf7b2ecd8c068d4678aee88810cfb120",
      "changeCount": 1,
      …
    },
    …
  ],
  "nextPageToken": "…"
}
```

### GetChangesets

Ids are base64. Results come back in request order; `include_changes` adds the change list (relative to the first parent).

```bash
TIP=$($G -d '{"revision":"main"}' $A xylem.v1.RepoService/ResolveRevision | jq -r .id)
$G -d "{\"ids\":[\"$TIP\"],\"include_changes\":true}" $A xylem.v1.RepoService/GetChangesets
```
```json
{ "changesets": [ { "gitSha": "1eae83c0bf…", "changeCount": 1,
  "changes": [ { "path": "UkVBRE1F", "op": "OP_PUT", "mode": "FILE_MODE_FILE", "size": "29",
                 "blobId": "2DzVgHD3J5kz/wWOTqDb60GqjKSVaSjZa24lQoja5OQ=" } ], … } ] }
```

### ListDirectory

Without `recursive`, it lists one directory's entries by name. With `recursive`, it lists every file below the directory with repository-relative paths, like `git ls-tree -r`. Page tokens pin the directory's tree, so later pages stay consistent even if `main` moves.

```bash
$G -d '{"revision":"main","page":{"page_size":3}}' $A xylem.v1.RepoService/ListDirectory
$G -d "{\"path\":\"$(b64 dup)\",\"recursive\":true}" $A xylem.v1.RepoService/ListDirectory
```
```json
{
  "treeId": "YtgLrRV1gtgQplFF5ml2PqdJwZxWvslI56S9c5846ug=",
  "entries": [
    { "name": "UkVBRE1F", "mode": "FILE_MODE_FILE", "id": "2DzVgHD3…", "size": "29" },
    { "name": "YmlnLmJpbg==", "mode": "FILE_MODE_FILE", "id": "C4dXpHp6…", "size": "11534336" },
    { "name": "ZHVw", "mode": "FILE_MODE_TREE", "id": "5+ADY3t5…" }
  ],
  "nextPageToken": "AWLYC60VdYLYEKZRReZpdj6nScGcVr7JSOekvXOfOOroAANkdXA"
}
```

The recursive call returns the names `ZHVwL2IudHh0` (`dup/b.txt`) and `ZHVwL2MvZC50eHQ=` (`dup/c/d.txt`).

### ReadFile

Streams the file: the first message carries `mode`, `size` and `blobId`, and content follows in chunks of at most 1 MiB.

```bash
$G -d "{\"path\":\"$(b64 README)\"}" $A xylem.v1.RepoService/ReadFile
```
```json
{ "mode": "FILE_MODE_FILE", "size": "29", "blobId": "2DzVgHD3J5kz/wWOTqDb60GqjKSVaSjZa24lQoja5OQ=",
  "chunk": "ZWRnZSBmaXh0dXJlCm1haW4gbW92ZXMKdGFpbAo=" }
```

`chunk` decodes to `edge fixture\nmain moves\ntail\n`.

### Diff

An empty `from_revision` means the first parent of `to_revision`. `include_hunks` adds unified hunks (`context_lines`, default 3). Statuses are `ADDED`, `DELETED`, `MODIFIED` and `MODE_CHANGED`. `paths` filters by path prefix.

```bash
$G -d '{"to_revision":"main","include_hunks":true}' $A xylem.v1.RepoService/Diff
$G -d "{\"from_revision\":\"main~19\",\"to_revision\":\"main\",\"paths\":[\"$(b64 x)\",\"$(b64 dup)\"]}" $A xylem.v1.RepoService/Diff
```
```json
{ "files": [ {
    "path": "UkVBRE1F", "status": "STATUS_MODIFIED",
    "oldMode": "FILE_MODE_FILE", "newMode": "FILE_MODE_FILE",
    "oldBlob": "G1ngkRGC…", "newBlob": "2DzVgHD3…",
    "hunks": [ { "oldStart": 1, "oldLines": 2, "newStart": 1, "newLines": 3,
                 "body": "IGVkZ2UgZml4dHVyZQogbWFpbiBtb3ZlcwordGFpbAo=" } ] } ] }
```

The hunk body decodes to ` edge fixture\n main moves\n+tail\n`. The filtered diff returns `dup/a.txt` as `STATUS_DELETED` and `x` as `STATUS_MODIFIED`.

### Blame

Each line comes back with its origin changeset and its line number there, following first-parent history (`git blame --first-parent` semantics; renames are not followed). `origins` holds the metadata of the distinct origin changesets.

```bash
$G -d "{\"path\":\"$(b64 README)\"}" $A xylem.v1.RepoService/Blame
```

Decoded with the `gitSha` of each origin:

```
552df637 1 edge fixture
06d9890a 2 main moves
1eae83c0 3 tail
```

### IsAncestor and MergeBase

```bash
$G -d '{"ancestor":"main~5","descendant":"main"}' $A xylem.v1.RepoService/IsAncestor     # { "result": true }
$G -d '{"a":"main~1^2","b":"main~1"}' $A xylem.v1.RepoService/MergeBase                 # { "ids": [ "rIyRgO/1…" ] }
```

`MergeBase` returns every best common ancestor, like `git merge-base --all`, sorted by generation.

### UpdateRef (admin)

An empty `expected_id` creates the ref. Otherwise the call is a compare-and-swap from `expected_id`. It must be a first-parent fast-forward unless `force` is set; a forced update rewinds main's mainline.

```bash
NEW=$($G -d '{"revision":"main~1"}' $A xylem.v1.RepoService/ResolveRevision | jq -r .id)
$G -d "{\"name\":\"demo/example\",\"new_id\":\"$NEW\"}" $A xylem.v1.RepoService/UpdateRef
```
```json
{ "ref": { "name": "demo/example", "id": "I8K6CtCeoIWwRg1V+DN0womNIEHAvnqzRD9qlWlCXIM=", "version": "1" } }
```

Running it again fails with `Aborted: conflict: ref "demo/example" already exists`.

## ObjectService

Objects are stored zstd frames of `header ‖ body`; the receiver verifies them against their ids. Up to 1,024 ids per call.

```bash
BLOB=2DzVgHD3J5kz/wWOTqDb60GqjKSVaSjZa24lQoja5OQ=
$G -d "{\"ids\":[\"$BLOB\",\"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"]}" $A xylem.v1.ObjectService/HasObjects
$G -d "{\"ids\":[\"$BLOB\"]}" $A xylem.v1.ObjectService/GetObjects
```
```json
{ "present": [ true, false ] }
{ "objects": [ { "id": "2DzVgHD3J5kz/wWOTqDb60GqjKSVaSjZa24lQoja5OQ=",
                 "data": "KLUv/QQAKQEAYmxvYiAyOQBlZGdlIGZpeHR1cmUKbWFpbiBtb3Zlcwp0YWlsCq09QNE=" } ] }
```

Ids that aren't found are listed in `missing`, in the final message.

`PutObjects` is a client stream of the same `StoredObject`s. The server verifies each object against its id and accepts **blobs only** (≤ 64 MiB), because trees and changesets are built server-side. A tree, a hash mismatch or an oversized blob fails the call with `InvalidArgument`. grpcurl reads a stream of JSON messages from stdin:

```bash
DATA=$($G -d "{\"ids\":[\"$BLOB\"]}" $A xylem.v1.ObjectService/GetObjects | jq -r '.objects[0].data')
echo "{\"objects\":[{\"id\":\"$BLOB\",\"data\":\"$DATA\"}]}" | $G -d @ $A xylem.v1.ObjectService/PutObjects
```
```json
{ "alreadyPresent": 1 }
```

Every `WriteService` RPC returns `Unimplemented` until the rest of the write path arrives (M6).

## Errors

Errors use the gRPC codes from 05 §Error model:

```text
$G -d '{"revision":"nope"}' $A xylem.v1.RepoService/ResolveRevision
  Code: NotFound          Message: not found: unknown revision "nope"
$G -d "{\"path\":\"$(b64 big.bin)\"}" $A xylem.v1.RepoService/Blame
  Code: FailedPrecondition  Message: binary file: "big.bin"
```

## Health and metrics (HTTP, :7401)

```bash
curl localhost:7401/healthz   # ok
curl localhost:7401/readyz    # ready (Postgres reachable, object store reachable)
curl -s localhost:7401/metrics | grep xylem_rpc_duration_seconds_count
```
