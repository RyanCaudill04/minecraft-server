#!/usr/bin/env bash
# Tests for pi-cleanup-legacy.sh against a made-up home folder.
#
# Usage: scripts/pi-cleanup-legacy_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
home=$(mktemp -d "${TEST_TMPDIR:-${TMPDIR:-/tmp}}/cleanup_test.XXXXXX")
sleeper=
trap '[ -z "$sleeper" ] || kill "$sleeper" 2>/dev/null || true; rm -rf "$home"' EXIT
failures=0

server=$home/minecraft_server
units=$home/.config/systemd/user
mkdir -p "$server/bin" "$server/deploy/systemd" "$server/world" "$server/world.backup-2026-01-01" \
  "$home/apps/current/deploy/systemd" "$units/default.target.wants"
touch "$server/bin/serve" "$server/bin/play" "$server/world/level.dat" "$server/world.backup-2026-01-01/level.dat"
printf '[Service]\nExecStart=%%h/minecraft_server/lazymc start\n' >"$server/deploy/systemd/minecraft.service"
ln -s "$server/deploy/systemd/minecraft.service" "$units/minecraft.service"
# play reads the server folder, but runs from the release: not a leftover.
printf '# reads ~/minecraft_server/world\n[Service]\nExecStart=%%h/apps/current/bin/play -server %%h/minecraft_server -addr x\n' \
  >"$home/apps/current/deploy/systemd/play.service"
ln -s "$home/apps/current/deploy/systemd/play.service" "$units/play.service"
ln -s "$units/play.service" "$units/default.target.wants/play.service"
# An old hand-made unit still running the old binary.
printf '[Service]\nExecStart=%%h/minecraft_server/bin/serve -dir x\n' >"$units/serve.service"

# run runs the script with args and keeps its exit status and output.
run() {
  status=0
  out=$(HOME=$home "$dir/pi-cleanup-legacy.sh" "$@" 2>&1) || status=$?
}

# check fails name unless the exit status is want and the output matches
# every remaining extended regexp.
check() {
  local name=$1 want=$2 pattern
  shift 2
  if [ "$status" != "$want" ]; then
    echo "FAIL $name: exit $status, want $want; output:"
    printf '    %s\n' "$out"
    failures=$((failures + 1))
    return
  fi
  for pattern in "$@"; do
    if ! grep -qE -- "$pattern" <<<"$out"; then
      echo "FAIL $name: output has no line matching $pattern; output:"
      printf '    %s\n' "$out"
      failures=$((failures + 1))
    fi
  done
}

# exists fails name unless every path exists; gone, unless none does.
exists() {
  local name=$1 path
  shift
  for path in "$@"; do
    [ -e "$path" ] || { echo "FAIL $name: $path was removed"; failures=$((failures + 1)); }
  done
}
gone() {
  local name=$1 path
  shift
  for path in "$@"; do
    [ ! -e "$path" ] || { echo "FAIL $name: $path still exists"; failures=$((failures + 1)); }
  done
}

run --bogus
check "unknown option" 2

run
check "dry run" 0 '^Dry run' "check +$units/serve.service" "keep +$server/bin: still used by serve.service" \
  "keep +$server/world.backup-2026-01-01 \(add --backups"
if grep -qE 'minecraft\.service|play\.service' <<<"$out"; then
  echo "FAIL dry run: flagged minecraft.service or play.service"
  failures=$((failures + 1))
fi
exists "dry run" "$server/bin" "$server/world.backup-2026-01-01" "$units/serve.service"

run --apply
check "apply while bin is in use" 1 "keep +$server/bin"
exists "apply while bin is in use" "$server/bin"

rm "$units/serve.service"
run
check "dry run, bin unused" 0 "ok +no unit but minecraft.service" "would remove $server/bin"
exists "dry run, bin unused" "$server/bin"

# A process still running from bin/ keeps it too.
printf '#!/bin/sh\nsleep 30 &\ntrap "kill $!; exit" TERM\nwait\n' >"$server/bin/sleeper"
chmod +x "$server/bin/sleeper"
"$server/bin/sleeper" &
sleeper=$!
run --apply
check "apply while a process runs from bin" 1 "keep +$server/bin: still used by a running process"
kill "$sleeper"
wait "$sleeper" 2>/dev/null || true
sleeper=

run --apply
check "apply" 0 "removed +$server/bin" "keep +$server/world.backup-2026-01-01"
gone "apply" "$server/bin"
exists "apply" "$server/world.backup-2026-01-01" "$server/world/level.dat" "$units/minecraft.service" "$units/play.service"

run --backups
check "dry run with backups" 0 "would remove $server/world.backup-2026-01-01"
exists "dry run with backups" "$server/world.backup-2026-01-01"

run --apply --backups
check "apply with backups" 0 "removed +$server/world.backup-2026-01-01"
gone "apply with backups" "$server/world.backup-2026-01-01"
exists "apply with backups" "$server/world/level.dat"

run --apply --backups
check "again" 0 "^Nothing to clean up\.$"

if [ "$failures" -gt 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "pi-cleanup-legacy: all tests passed"
