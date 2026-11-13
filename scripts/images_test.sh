#!/usr/bin/env bash
# Tests for images.sh: play and admin follow the release tag, and
# mc-server's tag changes only when something in its image does.
#
# Usage: scripts/images_test.sh
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failures=0
fail() { echo "FAIL $*"; failures=$((failures + 1)); }

# A copy of just what images.sh reads, plus a file it must ignore.
mkdir -p "$tmp/scripts" "$tmp/deploy/containers" "$tmp/web"
cp "$root/scripts/images.sh" "$root/scripts/fetch-server.sh" "$tmp/scripts/"
cp "$root/deploy/server.json" "$tmp/deploy/"
cp -R "$root/deploy/containers/mc-server" "$tmp/deploy/containers/"
echo 'package main' >"$tmp/web/main.go"

server() { "$tmp/scripts/images.sh" "$1" | sed -n 's/^MC_SERVER_IMAGE=//p'; }

out=$("$tmp/scripts/images.sh" r7)
grep -qx 'PLAY_IMAGE=ghcr.io/ryancaudill04/minecraft-server/play:r7' <<<"$out" || fail "play: $out"
grep -qx 'ADMIN_IMAGE=ghcr.io/ryancaudill04/minecraft-server/admin:r7' <<<"$out" || fail "admin: $out"
grep -qxE 'MC_SERVER_IMAGE=ghcr.io/ryancaudill04/minecraft-server/mc-server:inputs-[0-9a-f]{16}' <<<"$out" || fail "mc-server: $out"
[ "$(wc -l <<<"$out")" -eq 3 ] || fail "want three lines: $out"

base=$(server r7)
[ "$(server r8)" = "$base" ] || fail "a new release tag changed mc-server's tag"
echo '// changed' >>"$tmp/web/main.go"
[ "$(server r8)" = "$base" ] || fail "a web change changed mc-server's tag"

for f in deploy/server.json scripts/fetch-server.sh deploy/containers/mc-server/Dockerfile deploy/containers/mc-server/lazymc.toml; do
  cp "$tmp/$f" "$tmp/saved"
  echo '# changed' >>"$tmp/$f"
  [ "$(server r8)" != "$base" ] || fail "changing $f kept mc-server's tag"
  mv "$tmp/saved" "$tmp/$f"
done
[ "$(server r8)" = "$base" ] || fail "putting the files back didn't restore the tag"

touch "$tmp/deploy/containers/mc-server/new-file"
[ "$(server r8)" != "$base" ] || fail "a new file in mc-server/ kept mc-server's tag"
rm "$tmp/deploy/containers/mc-server/new-file"

[ "$(IMAGE_REPO=example.com/x "$tmp/scripts/images.sh" r7 | head -1)" = 'PLAY_IMAGE=example.com/x/play:r7' ] || fail "IMAGE_REPO"
if "$tmp/scripts/images.sh" '../x' >/dev/null 2>&1; then fail "a bad tag: want a failure"; fi

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "images: all cases pass"
