#!/usr/bin/env bash
# Runs the release's containers (compose.yaml) with docker compose.
# mc-containers.service runs "stack.sh up" on every deploy and
# "stack.sh stop-web" when it stops.
#
# Usage: deploy/containers/stack.sh up|stop-web|down|config
#
#   up        Start the images the release pins (deploy/images.env), pulling
#             any that are missing. A container whose image and settings
#             haven't changed keeps running, so a release that doesn't touch
#             mc-server's image doesn't restart the Minecraft server.
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
#   BACKUP_KEEP_DAYS  passed to admin
#   MC_HEAP           Paper's heap, passed to mc-server (default 10G)
#   APPS_DIR          mc-agent's folder, for admin (default ~/apps)
#   DOCKER_SOCKET     the Docker Engine API (default /var/run/docker.sock)
#   SECRETS_ENV       admin's secrets (default ~/.config/minecraft/secrets.env)
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
if [ -z "${DOCKER_GID:-}" ]; then
  DOCKER_GID=$(stat -c %g "$DOCKER_SOCKET" 2>/dev/null) ||
    die "can't read $DOCKER_SOCKET; is Docker installed and running?"
fi

compose=(docker compose --project-name minecraft --file "$here/compose.yaml" --env-file "$images")

case ${1:-} in
  up)
    # Docker would create a missing folder owned by root, and an empty
    # server folder would start a brand new world.
    [ -d "$SERVER_DIR" ] || die "no server folder at $SERVER_DIR; set SERVER_DIR in host.env"
    mkdir -p "$BACKUP_DIR" "$APPS_DIR"
    "${compose[@]}" up --detach --remove-orphans --pull missing --quiet-pull
    ;;
  stop-web) "${compose[@]}" stop play admin ;;
  down) "${compose[@]}" down ;;
  config) "${compose[@]}" config "${@:2}" ;;
  *)
    sed -n '6p' "$0" | sed 's/^# //' >&2
    exit 2
    ;;
esac
