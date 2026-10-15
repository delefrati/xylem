#!/usr/bin/env bash
# Clones pinned commits of real repositories into testdata/external/<name>
# (bare, gitignored) for `make parity-external` (10-testing.md §External).
# Each clone's refs/heads/main is set to the pinned sha, so results are
# reproducible. Re-running only fetches what is missing.
set -euo pipefail

cd "$(dirname "$0")/.."
DEST=testdata/external

# name  url  pinned sha
REPOS=(
	"cobra      https://github.com/spf13/cobra.git            adbc8813901bba65827259daa8e22ff94ec1f30e"
	"prometheus https://github.com/prometheus/prometheus.git  6e69e843b38d22ded60a13e0163f6326136f59fa"
)

only="${1:-}"
mkdir -p "$DEST"
for entry in "${REPOS[@]}"; do
	read -r name url sha <<<"$entry"
	if [[ -n "$only" && "$only" != "$name" ]]; then
		continue
	fi
	dir="$DEST/$name"
	if ! git -C "$dir" rev-parse --git-dir >/dev/null 2>&1; then
		echo "== cloning $url into $dir"
		git clone --bare --quiet --no-tags --single-branch --branch main "$url" "$dir"
	fi
	if ! git -C "$dir" cat-file -e "$sha^{commit}" 2>/dev/null; then
		echo "== fetching $sha"
		git -C "$dir" fetch --quiet --no-tags origin "$sha"
	fi
	git -C "$dir" update-ref refs/heads/main "$sha"
	echo "$name: main = $sha ($(git -C "$dir" rev-list --count main) commits)"
done
