#!/usr/bin/env bash
# make demo: the local walkthrough of 11-dev-environment.md. Starts services,
# imports the edge fixture into the "demo" repository, runs a server on
# :7410, then clones, edits, commits and lands with xy. Re-runnable.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$PWD
BIN=$ROOT/bin
WS=${XYLEM_DEMO_DIR:-/tmp/xylem-demo}
SPARSE=$WS-sparse
ADDR=localhost:7410
export XYLEM_AUTHOR=${XYLEM_AUTHOR:-"Xylem Demo <demo@example.com>"}

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
xy() {
	printf '$ xy'
	printf ' %q' "$@"
	printf '\n'
	"$BIN/xy" "$@"
}

step "services, build, migrations"
make --no-print-directory up >/dev/null
make --no-print-directory build >/dev/null
"$BIN/xylemd" migrate

step "import the edge fixture into repo \"demo\""
[ -d testdata/fixtures/edge ] || scripts/make-fixture-repo.sh testdata/fixtures/edge edge
# After a previous demo landed on top of the import, main is ahead of git and
# the importer (a mirror) refuses to move it; the import is already there.
if ! out=$("$BIN/xylemd" import --git testdata/fixtures/edge --branch main --repo demo 2>&1); then
	if grep -q "is not on the first-parent history of main" <<<"$out"; then
		echo "repo demo was imported by an earlier run and has demo changes on top; keeping it"
	else
		echo "$out" >&2
		exit 1
	fi
else
	echo "$out"
fi

step "server on $ADDR"
"$BIN/xylemd" server --listen :7410 --http-listen :7411 >"$ROOT/data/demo-server.log" 2>&1 &
SERVER=$!
trap 'kill $SERVER 2>/dev/null || true' EXIT
for _ in $(seq 50); do
	curl -sf localhost:7411/readyz >/dev/null && break
	sleep 0.1
done

step "clone (full) into $WS"
rm -rf "$WS" "$SPARSE"
xy --repo demo clone "$ADDR" "$WS"
cd "$WS"
xy log --oneline -n 3

step "edit, status, diff"
echo "hello from the demo" >>README
xy status
xy diff

step "commit and land"
xy commit -m "demo: say hello"
xy stack
xy land
xy log --oneline -n 3
xy blame README

step "sparse clone of dup/ into $SPARSE"
cd "$ROOT"
xy --repo demo clone "$ADDR" "$SPARSE" --sparse dup
cd "$SPARSE"
xy sparse list
xy status
ls -F

step "done: workspaces in $WS and $SPARSE (server stopped)"
