#!/usr/bin/env bash
# Builds the dev sites for Cloudflare (dev/): play's and admin's real pages
# plus a synthetic API snapshot, saved as api/<name>.json by the
# TestDevSnapshot tests in web/cmd/play and web/cmd/admin. dev/worker.mjs
# serves the snapshot in place of the API.
#
#   <output>/play    for dev-play.ryancaudill.dev
#   <output>/admin   for dev-admin.ryancaudill.dev
#
# Usage: scripts/build-dev.sh [output folder, default dev/dist]
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$root/dev/dist}
mkdir -p "$out"
out=$(cd "$out" && pwd)

for site in play admin; do
  rm -rf "${out:?}/$site"
  mkdir -p "$out/$site"
  cp -R "$root/sites/$site/." "$out/$site/"
  # The Go apps serve sites/shared at /shared/; here it's a plain folder.
  cp -R "$root/sites/shared" "$out/$site/shared"
  # go test prints failures on stdout, so keep it and show it if the run fails.
  if ! log=$(cd "$root/web" && DEV_SNAPSHOT_DIR="$out/$site" \
    go test -count=1 -run '^TestDevSnapshot$' "./cmd/$site" 2>&1); then
    printf '%s\n' "$log" >&2
    exit 1
  fi
  snapshots=("$out/$site/api/"*.json)
  echo "$site: ${#snapshots[@]} API snapshots in $out/$site"
done
