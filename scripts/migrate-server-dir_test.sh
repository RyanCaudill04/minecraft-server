#!/usr/bin/env bash
# Tests for migrate-server-dir.sh. Each case builds a fake checkout (a git
# repo with tracked config and an untracked world), a fake HOME and a
# stand-in systemctl, then runs the migration against them.
#
# Usage: scripts/migrate-server-dir_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/migrate-server-dir_test.XXXXXX")
trap 'rm -rf "$work"' EXIT
failures=0

fail() { echo "FAIL $*"; failures=$((failures + 1)); }

# A stand-in systemctl: minecraft.service is active only if $FAKE_ACTIVE.
bin=$work/bin
mkdir -p "$bin"
cat >"$bin/systemctl" <<'EOF'
#!/usr/bin/env bash
[ "$*" = "--user is-active --quiet minecraft.service" ] && [ -n "${FAKE_ACTIVE:-}" ]
EOF
chmod +x "$bin/systemctl"

# layout <name>: a fresh HOME with a checkout at $HOME/minecraft_server,
# laid out like the Pi's today.
layout() {
  home=$work/$1
  co=$home/minecraft_server
  mkdir -p "$co"/{scripts,web,deploy/systemd,config,plugins/Chunky,versions/26.2}
  cp "$dir/migrate-server-dir.sh" "$co/scripts/"
  echo 'package main' >"$co/web/main.go"
  echo '[Unit]' >"$co/deploy/systemd/minecraft.service"
  echo readme >"$co/README.md"
  printf 'world*/\nlogs/\nlibraries/\nlazymc\nserver.properties\n' >"$co/.gitignore"
  echo '#!/bin/bash' >"$co/start.sh"
  echo 'directory = "."' >"$co/lazymc.toml"
  echo '[]' >"$co/ops.json"
  echo 'paper: 1' >"$co/config/paper-global.yml"
  echo 'chunky: 1' >"$co/plugins/Chunky/config.yml"
  echo jar >"$co/plugins/Chunky.jar"
  echo paper >"$co/versions/26.2/paper-26.2.jar"
  git -C "$co" init -q
  git -C "$co" add -A
  git -C "$co" -c user.name=t -c user.email=t@example.com commit -qm init
  # Runtime files, as the server leaves them.
  mkdir -p "$co"/{world/region,world_nether,logs,libraries/x,plugins/spark}
  echo level >"$co/world/level.dat"
  echo region >"$co/world/region/r.0.0.mca"
  echo nether >"$co/world_nether/level.dat"
  echo log >"$co/logs/latest.log"
  echo lib >"$co/libraries/x/a.jar"
  echo spark >"$co/plugins/spark.jar"
  echo data >"$co/plugins/spark/config.json"
  echo bin >"$co/lazymc"
  echo 'rcon.password=x' >"$co/server.properties"
  echo '[{"name":"a"}]' >"$co/usercache.json"
  mkdir -p "$home/.config/systemd/user"
  ln -s "$co/deploy/systemd/minecraft.service" "$home/.config/systemd/user/minecraft.service"
}

migrate() { HOME=$home PATH="$bin:$PATH" SERVER_DIR='' "$co/scripts/migrate-server-dir.sh" "$@"; }
snapshot() { (cd "$home" && find . | LC_ALL=C sort); }

# With no options, the server folder is the checkout: nothing to do.
layout today
before=$(snapshot)
out=$(migrate --apply 2>&1) || fail "today: exit $?: $out"
grep -q "today's layout" <<<"$out" || fail "today: should say it's today's layout: $out"
[ "$before" = "$(snapshot)" ] || fail "today: changed files"

# A dry run prints the plan and changes nothing.
layout dryrun
before=$(snapshot)
out=$(migrate --move-checkout-to "$home/src/minecraft-server" 2>&1) || fail "dry run: exit $?: $out"
[ "$before" = "$(snapshot)" ] || fail "dry run: changed files"
for want in 'move  world' 'move  world_nether' 'move  logs' 'move  libraries' 'move  lazymc' \
  'move  server.properties' 'move  usercache.json' 'copy  plugins' 'copy  config' 'copy  start.sh' \
  'copy  versions' 'keep  web' 'keep  scripts' 'keep  deploy' 'keep  .git' 'Dry run'; do
  grep -qF "$want" <<<"$out" || fail "dry run: no '$want' line: $out"
done

# The recommended move: the checkout moves, the server folder stays.
layout move
new=$home/src/minecraft-server
srv=$home/minecraft_server
out=$(migrate --move-checkout-to "$new" --apply 2>&1) || fail "move: exit $?: $out"
[ "$(cat "$srv/world/region/r.0.0.mca" 2>/dev/null)" = region ] || fail "move: world not in the server folder"
for f in world_nether/level.dat logs/latest.log libraries/x/a.jar lazymc server.properties \
  usercache.json start.sh lazymc.toml ops.json config/paper-global.yml plugins/Chunky.jar \
  plugins/Chunky/config.yml plugins/spark.jar plugins/spark/config.json versions/26.2/paper-26.2.jar; do
  [ -f "$srv/$f" ] || fail "move: $f not in the server folder"
done
for f in web scripts deploy README.md .git; do
  [ ! -e "$srv/$f" ] || fail "move: repo-only $f was copied to the server folder"
  [ -e "$new/$f" ] || fail "move: $f missing from the new checkout"
done
for f in world logs libraries lazymc server.properties plugins/spark.jar plugins/spark; do
  [ ! -e "$new/$f" ] || fail "move: $f is still in the checkout"
done
[ -z "$(git -C "$new" status --porcelain)" ] || fail "move: checkout isn't clean: $(git -C "$new" status --porcelain)"
[ "$(readlink "$home/.config/systemd/user/minecraft.service")" = "$new/deploy/systemd/minecraft.service" ] ||
  fail "move: unit not relinked"
archive=$(find "$home/backups/minecraft" -name 'pre-migrate-*.tar.gz' 2>/dev/null | head -n 1)
if [ -n "$archive" ]; then
  tar -tzf "$archive" | grep -q '^world/level.dat$' || fail "move: backup has no world/level.dat"
  tar -tzf "$archive" | grep -q '^ops.json$' || fail "move: backup has no ops.json"
  tar -tzf "$archive" | grep -q '^libraries' && fail "move: backup shouldn't hold libraries/"
else
  fail "move: no backup written"
fi

# A separate server folder, keeping the checkout where it is.
layout separate
srv=$home/minecraft
out=$(migrate --server-dir "$srv" --no-backup --apply 2>&1) || fail "separate: exit $?: $out"
[ -f "$srv/world/level.dat" ] || fail "separate: world not moved"
[ ! -e "$co/world" ] || fail "separate: world still in the checkout"
grep -q "SERVER_DIR=$srv" <<<"$out" || fail "separate: should say to set SERVER_DIR: $out"
[ ! -e "$home/backups" ] || fail "separate: --no-backup still wrote a backup"
[ "$(readlink "$home/.config/systemd/user/minecraft.service")" = "$co/deploy/systemd/minecraft.service" ] ||
  fail "separate: unit link should be unchanged"

# Refuses while the server runs.
layout running
before=$(snapshot)
if out=$(FAKE_ACTIVE=1 migrate --move-checkout-to "$home/src/x" --apply 2>&1); then
  fail "running: want a non-zero exit"
else
  grep -q 'minecraft.service is running' <<<"$out" || fail "running: wrong error: $out"
fi
[ "$before" = "$(snapshot)" ] || fail "running: changed files"

# Refuses to move into a folder that already has files.
layout nonempty
mkdir -p "$home/minecraft"
echo x >"$home/minecraft/keep.txt"
if migrate --server-dir "$home/minecraft" --apply >/dev/null 2>&1; then
  fail "non-empty: want a non-zero exit"
fi
[ -f "$co/world/level.dat" ] || fail "non-empty: moved the world anyway"

# Refuses a new checkout path that exists.
layout exists
mkdir -p "$home/src/minecraft-server"
if migrate --move-checkout-to "$home/src/minecraft-server" --apply >/dev/null 2>&1; then
  fail "exists: want a non-zero exit"
fi

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "migrate-server-dir: all cases pass"
