#!/usr/bin/env bash
# Tests for deploy/containers/stack.sh, with a stand-in docker that records
# its arguments and the variables compose would read.
#
# Usage: scripts/stack_test.sh
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failures=0
fail() { echo "FAIL $*"; failures=$((failures + 1)); }

# A release as mc-agent unpacks it.
rel=$tmp/apps/releases/r9
mkdir -p "$rel/deploy/containers" "$tmp/bin" "$tmp/home/minecraft_server"
cp "$root/deploy/containers/compose.yaml" "$root/deploy/containers/stack.sh" "$rel/deploy/containers/"
printf 'PLAY_IMAGE=example/play:r9\n' >"$rel/deploy/images.env"

cat >"$tmp/bin/docker" <<'STUB'
#!/usr/bin/env bash
{
  echo "args: $*"
  for v in SERVER_DIR BACKUP_DIR APPS_DIR SECRETS_ENV DOCKER_SOCKET DOCKER_GID HOST_UID HOST_GID; do
    echo "$v=${!v-unset}"
  done
} >"$DOCKER_LOG"
STUB
chmod +x "$tmp/bin/docker"

run() {
  env -i PATH="$tmp/bin:$PATH" HOME="$tmp/home" DOCKER_LOG="$tmp/log" DOCKER_GID=990 "$@" \
    "$rel/deploy/containers/stack.sh" "${stack_args[@]}"
}
has() { grep -qxF -- "$1" "$tmp/log" || fail "$2: want '$1' in: $(cat "$tmp/log")"; }

compose="compose --project-name minecraft --file $rel/deploy/containers/compose.yaml --env-file $rel/deploy/images.env"

stack_args=(up)
run >/dev/null
has "args: $compose up --detach --remove-orphans --pull missing --quiet-pull" "up"
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

# A missing server folder must stop everything: Docker would create an
# empty one, and Paper would start a new world in it.
rm -f "$tmp/log"
if out=$(run SERVER_DIR="$tmp/missing" 2>&1); then
  fail "missing server folder: want a failure"
fi
grep -q "no server folder" <<<"$out" || fail "missing server folder: wrong error: $out"
[ ! -e "$tmp/log" ] || fail "missing server folder: docker ran anyway"
[ ! -e "$tmp/missing" ] || fail "missing server folder: it was created"

stack_args=(stop-web)
run >/dev/null
has "args: $compose stop play admin" "stop-web leaves mc-server running"

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
