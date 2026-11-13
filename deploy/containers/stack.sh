#!/usr/bin/env bash
# Runs the release's containers (compose.yaml) with docker compose.
# mc-containers.service runs "stack.sh up" on every deploy and at boot, and
# "stack.sh stop-web" when it stops.
#
# Usage: deploy/containers/stack.sh up|stop-web|down|config
#
#   up        Start the images the release pins (deploy/images.env), pulling
#             any that are missing, then check that LazyMC answers and
#             remove this repo's images that no container uses. A container
#             whose image and settings haven't changed keeps running, so a
#             release that doesn't touch mc-server's image doesn't restart
#             the Minecraft server. An mc-server stopped on purpose (from
#             admin, or docker stop) since the Pi booted stays stopped: it
#             is only updated, and starts with its new image when someone
#             starts it. Fails if LazyMC doesn't come up, which fails the
#             deploy, so mc-agent rolls back.
#   stop-web  Stop play and admin, and leave mc-server running. This is the
#             unit's stop: a rollback to a release from before containers
#             starts its own play and admin, and players stay connected.
#   down      Stop and remove all three, the Minecraft server included.
#   config    Print compose.yaml with every variable filled in; any further
#             arguments go to docker compose config (e.g. --format json).
#
# Host settings come from the environment; mc-containers.service loads
# ~/.config/minecraft/host.env, the file mc-backup reads too:
#   SERVER_DIR        the server folder (default ~/minecraft_server;
#                     MC_SERVER_DIR wins). It must exist: an empty one
#                     would start a new world.
#   BACKUP_DIR        mc-backup's archives, for admin (default
#                     ~/backups/minecraft; MC_BACKUP_DIR wins)
#   BACKUP_KEEP_DAYS  passed to admin (MC_BACKUP_KEEP_DAYS wins, as in
#                     mc-backup.sh)
#   MC_HEAP           Paper's heap, passed to mc-server (default 10G)
#   APPS_DIR          mc-agent's folder, for admin (default ~/apps)
#   DOCKER_SOCKET     the Docker Engine API (default /var/run/docker.sock)
#   SECRETS_ENV       admin's secrets (default ~/.config/minecraft/secrets.env)
# For scripts/stack_test.sh: MC_PORT (LazyMC's port, default 25565),
# STACK_BOOT_TIME (seconds since the epoch), STACK_WAIT (seconds to wait for
# Docker and for LazyMC, default 120 and 60).
set -euo pipefail

die() { echo "stack.sh: $*" >&2; exit 1; }

here=$(cd "$(dirname "$0")" && pwd)
images=$(cd "$here/.." && pwd)/images.env
[ -f "$images" ] || die "no $images; this release has no pinned images"

export SERVER_DIR=${MC_SERVER_DIR:-${SERVER_DIR:-$HOME/minecraft_server}}
export BACKUP_DIR=${MC_BACKUP_DIR:-${BACKUP_DIR:-$HOME/backups/minecraft}}
export APPS_DIR=${APPS_DIR:-$HOME/apps}
export SECRETS_ENV=${SECRETS_ENV:-${XDG_CONFIG_HOME:-$HOME/.config}/minecraft/secrets.env}
export DOCKER_SOCKET=${DOCKER_SOCKET:-/var/run/docker.sock}
export HOST_UID HOST_GID DOCKER_GID
HOST_UID=$(id -u)
HOST_GID=$(id -g)
mc_port=${MC_PORT:-25565}

compose=(docker compose --project-name minecraft --file "$here/compose.yaml" --env-file "$images")
image_label=org.opencontainers.image.source=https://github.com/RyanCaudill04/minecraft-server

# wait_for_docker waits for the Docker daemon, which may still be starting
# when the unit runs at boot, and reads the socket's group for admin.
wait_for_docker() {
  local i
  for ((i = 0; i < ${STACK_WAIT:-120}; i += 2)); do
    docker info >/dev/null 2>&1 && break
    sleep 2
  done
  docker info >/dev/null 2>&1 || die "Docker doesn't answer on $DOCKER_SOCKET; is it running, and is $(id -un) in the docker group?"
  if [ -z "${DOCKER_GID:-}" ]; then
    DOCKER_GID=$(stat -c %g "$DOCKER_SOCKET" 2>/dev/null) || die "can't read $DOCKER_SOCKET's group"
  fi
}

# to_epoch turns a docker inspect time (2026-11-13T04:45:34.123Z) into
# seconds, with GNU date (the Pi) or BSD date (macOS, for the tests).
to_epoch() {
  local t=${1%%.*}
  t=${t%Z}
  date -u -d "${t}Z" +%s 2>/dev/null || date -u -j -f '%Y-%m-%dT%H:%M:%S' "$t" +%s 2>/dev/null
}

boot_time() {
  if [ -n "${STACK_BOOT_TIME:-}" ]; then
    echo "$STACK_BOOT_TIME"
  else
    awk '/^btime /{print $2}' /proc/stat
  fi
}

# stopped_on_purpose reports whether mc-server exists, isn't running, and
# was meant to be off: it stopped after the Pi last booted (Docker's
# restart policy restarts a crash by itself, so that's a stop from admin or
# docker stop), or an earlier up updated it without starting it. After a
# reboot it starts again, as minecraft.service did.
stopped_on_purpose() {
  local status finished stopped booted
  read -r status finished < <(docker inspect -f '{{.State.Status}} {{.State.FinishedAt}}' mc-server 2>/dev/null) || return 1
  case $status in
    created) return 0 ;;
    exited | dead) ;;
    *) return 1 ;;
  esac
  stopped=$(to_epoch "$finished") || return 1
  booted=$(boot_time) || return 1
  [ -n "$stopped" ] && [ -n "$booted" ] && [ "$stopped" -ge "$booted" ]
}

running_restarts() { docker inspect -f '{{.State.Running}} {{.RestartCount}}' mc-server 2>/dev/null; }
lazymc_answers() { (exec 3<>"/dev/tcp/127.0.0.1/$mc_port") 2>/dev/null; }

# check_server waits for mc-server to run and LazyMC to answer on its port,
# then makes sure it isn't restarting in a loop. play's and admin's health
# checks pass with the server offline, so this is the deploy's check on it.
check_server() {
  local i before
  for ((i = 0; i < ${STACK_WAIT:-60}; i++)); do
    before=$(running_restarts)
    if [ "${before%% *}" = true ] && lazymc_answers; then
      sleep 3
      [ "$(running_restarts)" = "$before" ] && return 0
    fi
    sleep 1
  done
  docker logs --tail 20 mc-server >&2 || true
  die "mc-server isn't running, or nothing answers on port $mc_port"
}

# prune removes this repo's images that no container uses, such as the
# ones older releases pinned, so they don't fill the disk the world and
# backups share. A rollback pulls what it needs again.
prune() {
  docker image prune --all --force --filter "label=$image_label" >/dev/null ||
    echo "stack.sh: couldn't remove old images" >&2
}

case ${1:-} in
  up)
    # Docker would create a missing folder owned by root, and an empty
    # server folder would start a brand new world.
    [ -d "$SERVER_DIR" ] || die "no server folder at $SERVER_DIR; set SERVER_DIR in host.env"
    mkdir -p "$BACKUP_DIR" "$APPS_DIR"
    wait_for_docker
    if stopped_on_purpose; then
      echo "mc-server was stopped on purpose, so it's updated but left stopped"
      "${compose[@]}" up --detach --remove-orphans --pull missing --quiet-pull play admin
      "${compose[@]}" up --no-start --pull missing --quiet-pull mc-server
    else
      "${compose[@]}" up --detach --remove-orphans --pull missing --quiet-pull
      check_server
    fi
    prune
    ;;
  stop-web)
    # No waiting here: at shutdown Docker may already be stopping, and the
    # socket's group only matters for starting admin.
    DOCKER_GID=${DOCKER_GID:-$(stat -c %g "$DOCKER_SOCKET" 2>/dev/null || echo 0)}
    "${compose[@]}" stop play admin
    ;;
  down) wait_for_docker && "${compose[@]}" down ;;
  config)
    DOCKER_GID=${DOCKER_GID:-0}
    "${compose[@]}" config "${@:2}"
    ;;
  *)
    sed -n '6p' "$0" | sed 's/^# //' >&2
    exit 2
    ;;
esac
