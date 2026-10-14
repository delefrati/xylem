#!/usr/bin/env bash
# Generate a deterministic git fixture repository (10-testing.md §Fixture repositories).
#
#   scripts/make-fixture-repo.sh <dir> tiny|edge
#
# Identities, dates and contents are fixed, so the same profile always yields
# the same commit shas. The user's git config is ignored.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <dir> tiny|edge" >&2
  exit 2
fi
dir=$1
profile=$2

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME="Ada Lovelace" GIT_AUTHOR_EMAIL="ada@example.com"
export GIT_COMMITTER_NAME="Ada Lovelace" GIT_COMMITTER_EMAIL="ada@example.com"
export LC_ALL=C

rm -rf "$dir"
mkdir -p "$dir"
cd "$dir"
git init -q -b main
git config core.autocrlf false
git config core.fileMode true
git config core.symlinks true
git config commit.gpgSign false

t0=1700000000
n=0
# at [tz]: set author and committer dates for the next commit (one minute apart).
at() {
  n=$((n + 1))
  local tz=${1:-+0000}
  export GIT_AUTHOR_DATE="@$((t0 + n * 60)) $tz" GIT_COMMITTER_DATE="@$((t0 + n * 60)) $tz"
}
commit() { # commit <message> [tz]
  at "${2:-+0000}"
  git add -A
  git commit -q --allow-empty-message -m "$1"
}

tiny() {
  echo "hello" > README
  mkdir -p src
  printf 'package main\n\nfunc main() {}\n' > src/main.go
  commit "initial"
  echo "hello world" > README; commit "update readme"
  printf 'package main\n\nfunc helper() {}\n' > src/helper.go; commit "add helper"
  printf 'line 1\nline 2\n' > notes.txt; commit "add notes"
  printf 'line 1\nline 2\nline 3\n' > notes.txt; commit "extend notes"
  rm src/helper.go; commit "remove helper"
  mkdir -p docs; echo "# Docs" > docs/index.md; commit "add docs"
  echo "more" >> docs/index.md; commit "more docs"
  rm notes.txt; echo "todo" > TODO; commit "notes to todo"
  echo "bye" >> README; commit "final"
}

edge() {
  # Root commit: assorted names, an empty file, duplicate contents, an exec script.
  echo "edge fixture" > README
  : > empty.txt
  mkdir -p dup/c "path with spaces" "ünïcødé"
  for p in dup/a.txt dup/b.txt dup/c/d.txt "path with spaces/file one.txt"; do echo "same content" > "$p"; done
  echo "unicode" > "ünïcødé/ファイル.txt"
  echo "latin1 name" > $'latin1-\xe9.txt'
  echo "newline name" > $'new\nline.txt'
  printf '#!/bin/sh\necho run\n' > run.sh; chmod 755 run.sh
  echo "x is a file" > x
  mkdir -p gone/deep; echo "1" > gone/deep/one; echo "2" > gone/two
  commit "root: assorted paths" +0530

  chmod 644 run.sh; commit "exec bit off"
  chmod 755 run.sh; echo "echo again" >> run.sh; commit "exec bit on and content" -0300

  rm x; ln -s README x; commit "x: file -> symlink"
  rm x; mkdir x; echo "inner" > x/inner.txt; commit "x: symlink -> directory"
  rm -r x; echo "x is a file again" > x; commit "x: directory -> file"

  git mv dup/a.txt renamed.txt; commit "rename (no rename detection: D + A)"
  rm -r gone; commit "delete a directory tree"

  head -c 11534336 /dev/zero | openssl enc -aes-256-ctr -nosalt -pass pass:xylem-fixture -pbkdf2 > big.bin
  commit "large binary (11 MiB)"

  at; git commit -q --allow-empty -m "empty commit"

  # The message goes through stdin: never write outside $dir (fixtures are
  # generated concurrently into sibling directories).
  at; echo "encoded" > enc.txt; git add enc.txt
  printf 'caf\xe9 au lait\n\nISO-8859-1 message body\n' | git -c i18n.commitEncoding=ISO-8859-1 commit -q -F -

  # Submodule entry (gitlink): added, changed, removed. The importer skips it.
  at; git update-index --add --cacheinfo "160000,$(git rev-parse HEAD),vendor/sub"
  echo "with submodule" > sub.txt; git add sub.txt; git commit -q -m "add submodule gitlink"
  at; git update-index --cacheinfo "160000,$(git rev-parse HEAD),vendor/sub"; git commit -q -m "move submodule"
  at; git update-index --force-remove vendor/sub; git commit -q -m "remove submodule"

  # A side branch; merging main into it makes a merge whose first parent is
  # the side branch, and main then fast-forwards to it (as `git pull` does).
  git checkout -q -b side
  echo "side 1" > side.txt; commit "side: 1"
  git checkout -q main
  echo "main moves" >> README; commit "main moves on"
  git checkout -q side
  echo "side 2" >> side.txt; commit "side: 2"
  at; git merge -q --no-edit --no-ff main -m "merge main into side"
  git checkout -q main
  git merge -q --ff-only side

  # Octopus merge of three branches.
  for b in o1 o2 o3; do
    git checkout -q -b "$b" main
    echo "$b" > "$b.txt"; commit "octopus branch $b"
  done
  git checkout -q main
  echo "main before octopus" > pre-octopus.txt; commit "main before octopus"
  at; git merge -q --no-edit o1 o2 o3 -m "octopus merge" >/dev/null

  # An unrelated history (second root) merged in.
  git checkout -q --orphan other
  git rm -q -rf --cached . >/dev/null
  find . -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
  mkdir -p imported; echo "other root" > imported/README; commit "other: root" -0800
  echo "other 2" > imported/two; commit "other: 2" -0800
  git checkout -q -f main
  at; git merge -q --no-edit --allow-unrelated-histories other -m "merge unrelated history"

  echo "tail" >> README; commit "tail"
}

case $profile in
  tiny) tiny ;;
  edge) edge ;;
  *) echo "unknown profile $profile (want tiny or edge)" >&2; exit 2 ;;
esac
git gc -q --prune=now
echo "$profile: $(git rev-list --count main) commits, main=$(git rev-parse main)"
