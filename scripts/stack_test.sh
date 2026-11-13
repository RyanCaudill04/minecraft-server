#!/usr/bin/env bash
# Tests for deploy/containers/stack.sh, with a stand-in docker that records
# every call (and, for compose, the variables compose would read) and plays
# back a state for the mc-server container. A small listener stands in for
# LazyMC's port.
#
# Usage: scripts/stack_test.sh
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
listener=''
trap '[ -z "$listener" ] || kill "$listener" 2>/dev/null; rm -rf "$tmp"' EXIT
failures=0
fail() { echo "FAIL $*"; failures=$((failures + 1)); }

# A release as mc-agent unpacks it.
rel=$tmp/apps/releases/r9
mkdir -p "$rel/deploy/containers" "$tmp/bin" "$tmp/home/minecraft_server"
cp "$root/deploy/containers/compose.yaml" "$root/deploy/containers/stack.sh" "$rel/deploy/containers/"
printf 'PLAY_IMAGE=example/play:r9\n' >"$rel/deploy/images.env"

# FAKE_STATE is what "docker inspect" says about mc-server ("<status>
# <finished at>"); empty means there's no such container. FAKE_RUNNING is
# "<running> <restart count>".
cat >"$tmp/bin/docker" <<'STUB'
#!/usr/bin/env bash
echo "args: $*" >>"$DOCKER_LOG"
case $1 in
  info) exit "${FAKE_INFO:-0}" ;;
  inspect)
    # FAKE_STATE is the state before up; after up the container runs
    # (FAKE_RUNNING), whatever it was before.
    case $3 in
      *Status*) [ -n "${FAKE_STATE:-}" ] || exit 1; echo "$FAKE_STATE" ;;
      *Running*) echo "${FAKE_RUNNING:-true 0}" ;;
    esac
    ;;
  compose)
    for v in SERVER_DIR BACKUP_DIR APPS_DIR SECRETS_ENV DOCKER_SOCKET DOCKER_GID HOST_UID HOST_GID; do
      echo "$v=${!v-unset}"
    done >>"$DOCKER_LOG"
    ;;
  logs) echo "fake mc-server log" ;;
esac
STUB
chmod +x "$tmp/bin/docker"

# A port something listens on, and one nothing does.
python3 -c '
import socket, sys, time
s = socket.socket(); s.bind(("127.0.0.1", 0)); s.listen(16)
print(s.getsockname()[1], flush=True)
while True:
    c, _ = s.accept(); c.close()
' >"$tmp/port" &
listener=$!
for _ in $(seq 50); do [ -s "$tmp/port" ] && break; sleep 0.1; done
open_port=$(cat "$tmp/port")
closed_port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')

boot=1794528000 # 2026-11-13T00:00:00Z
before_boot=2026-11-12T23:00:00.5Z
after_boot=2026-11-13T04:45:34.123456789Z

run() {
  rm -f "$tmp/log"
  env -i PATH="$tmp/bin:$PATH" HOME="$tmp/home" DOCKER_LOG="$tmp/log" DOCKER_GID=990 \
    MC_PORT="$open_port" STACK_BOOT_TIME="$boot" STACK_WAIT=4 "$@" \
    "$rel/deploy/containers/stack.sh" "${stack_args[@]}"
}
has() { grep -qxF -- "$1" "$tmp/log" || fail "$2: want '$1' in: $(cat "$tmp/log")"; }
# hasnt matches whole lines too, except a trailing "*" matches any rest.
hasnt() {
  local found
  if [[ $1 == *'*' ]]; then found=$(grep -F -- "${1%'*'}" "$tmp/log" 2>/dev/null || true)
  else found=$(grep -xF -- "$1" "$tmp/log" 2>/dev/null || true); fi
  [ -z "$found" ] || fail "$2: didn't want '$1' in: $(cat "$tmp/log")"
}

compose="compose --project-name minecraft --file $rel/deploy/containers/compose.yaml --env-file $rel/deploy/images.env"
up_all="args: $compose up --detach --remove-orphans --pull missing --quiet-pull"
prune='args: image prune --all --force --filter label=org.opencontainers.image.source=https://github.com/RyanCaudill04/minecraft-server'

stack_args=(up)

# First deploy: no mc-server yet, so everything starts, LazyMC answers,
# and old images are pruned.
run >/dev/null || fail "first up: failed"
has "$up_all" "first up"
has "$prune" "first up prunes"
has "SERVER_DIR=$tmp/home/minecraft_server" "default server folder"
has "BACKUP_DIR=$tmp/home/backups/minecraft" "default backup folder"
has "APPS_DIR=$tmp/home/apps" "default apps folder"
has "SECRETS_ENV=$tmp/home/.config/minecraft/secrets.env" "default secrets"
has "DOCKER_SOCKET=/var/run/docker.sock" "default socket"
has "DOCKER_GID=990" "docker group"
has "HOST_UID=$(id -u)" "uid"
has "HOST_GID=$(id -g)" "gid"
[ -d "$tmp/home/backups/minecraft" ] || fail "up: didn't make the backup folder"

# The MC_ names win, as in mc-backup.sh.
mkdir -p "$tmp/other"
run MC_SERVER_DIR="$tmp/other" SERVER_DIR=/nope MC_BACKUP_DIR="$tmp/bk" BACKUP_DIR=/nope >/dev/null
has "SERVER_DIR=$tmp/other" "MC_SERVER_DIR wins"
has "BACKUP_DIR=$tmp/bk" "MC_BACKUP_DIR wins"

# mc-server running: a normal update.
run FAKE_STATE="running 0001-01-01T00:00:00Z" >/dev/null || fail "running: failed"
has "$up_all" "running"

# Stopped since this boot (from admin or docker stop): play and admin are
# updated, mc-server is updated without starting, and nothing waits for
# LazyMC (whose port is closed here).
out=$(run FAKE_STATE="exited $after_boot" MC_PORT="$closed_port") || fail "stopped on purpose: failed: $out"
grep -q "left stopped" <<<"$out" || fail "stopped on purpose: didn't say so: $out"
has "args: $compose up --detach --remove-orphans --pull missing --quiet-pull play admin" "stopped on purpose: play and admin"
has "args: $compose up --no-start --pull missing --quiet-pull mc-server" "stopped on purpose: mc-server not started"
hasnt "$up_all" "stopped on purpose"
has "$prune" "stopped on purpose prunes"

# Updated but not started by an earlier up: still stopped on purpose.
run FAKE_STATE="created 0001-01-01T00:00:00Z" MC_PORT="$closed_port" >/dev/null || fail "created: failed"
has "args: $compose up --no-start --pull missing --quiet-pull mc-server" "created stays stopped"

# Stopped before the Pi booted (the shutdown stopped it): start it, as
# minecraft.service did at boot.
run FAKE_STATE="exited $before_boot" >/dev/null || fail "stopped before boot: failed"
has "$up_all" "stopped before boot starts"

# LazyMC never answers: the deploy fails, with mc-server's log.
if out=$(run MC_PORT="$closed_port" 2>&1); then fail "LazyMC down: want a failure"; fi
grep -q "nothing answers on port $closed_port" <<<"$out" || fail "LazyMC down: wrong error: $out"
grep -q "fake mc-server log" <<<"$out" || fail "LazyMC down: no log in: $out"
hasnt "$prune" "a failed up doesn't prune"

# mc-server isn't running (a crash loop): the deploy fails.
if run FAKE_STATE="restarting 0001-01-01T00:00:00Z" FAKE_RUNNING="false 7" >/dev/null 2>&1; then
  fail "not running: want a failure"
fi

# Docker never answers: a clear failure, and compose never runs.
if out=$(run FAKE_INFO=1 STACK_WAIT=2 2>&1); then fail "no Docker: want a failure"; fi
grep -q "Docker doesn't answer" <<<"$out" || fail "no Docker: wrong error: $out"
hasnt "args: compose*" "no Docker"

# A missing server folder must stop everything: Docker would create an
# empty one, and Paper would start a new world in it.
if out=$(run SERVER_DIR="$tmp/missing" 2>&1); then
  fail "missing server folder: want a failure"
fi
grep -q "no server folder" <<<"$out" || fail "missing server folder: wrong error: $out"
[ ! -e "$tmp/log" ] || fail "missing server folder: docker ran anyway"
[ ! -e "$tmp/missing" ] || fail "missing server folder: it was created"

stack_args=(stop-web)
run >/dev/null
has "args: $compose stop play admin" "stop-web leaves mc-server running"
hasnt "args: info*" "stop-web doesn't wait for Docker"

stack_args=(down)
run >/dev/null
has "args: $compose down" "down"

stack_args=(config --format json)
run >/dev/null
has "args: $compose config --format json" "config passes its flags on"

stack_args=(bogus)
if run >/dev/null 2>&1; then fail "unknown command: want a failure"; fi

# A release without pinned images can't run.
rm "$rel/deploy/images.env"
stack_args=(up)
if out=$(run 2>&1); then fail "no images.env: want a failure"; fi
grep -q "no pinned images" <<<"$out" || fail "no images.env: wrong error: $out"

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "stack: all cases pass"
