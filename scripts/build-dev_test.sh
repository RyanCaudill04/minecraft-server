#!/usr/bin/env bash
# Tests for build-dev.sh: it builds both dev sites, and every API route a
# page reads with GET has a snapshot, so the dev site never shows a
# section as broken just because the snapshot is missing it.
#
# Usage: scripts/build-dev_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$dir/.." && pwd)
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
failures=0

fail() {
  echo "FAIL $*"
  failures=$((failures + 1))
}

"$dir/build-dev.sh" "$out" >/dev/null

for site in play admin; do
  for f in index.html app.js style.css shared/base.css; do
    [ -f "$out/$site/$f" ] || fail "$site: $f missing"
  done
  # Routes the page names as a plain "/api/<name>" string. Admin's
  # console is POST only; the Worker refuses every POST.
  routes=$(grep -ohE '"/api/[a-z]+"' "$root/sites/$site/app.js" | tr -d '"' | sort -u | grep -vx '/api/console' || true)
  [ -n "$routes" ] || fail "$site: found no API routes in app.js"
  for route in $routes; do
    f="$out/$site/api/${route#/api/}.json"
    if [ ! -f "$f" ]; then
      fail "$site: $route has no snapshot (add it to TestDevSnapshot in web/cmd/$site)"
    elif ! jq -e . "$f" >/dev/null; then
      fail "$site: $route snapshot is not JSON"
    fi
  done
done

if [ "$failures" -gt 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "build-dev.sh OK"
