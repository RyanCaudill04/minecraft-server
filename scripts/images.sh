#!/usr/bin/env bash
# Prints the container images a release pins, in the env-file form that
# deploy/containers/stack.sh reads from a release's deploy/images.env:
#
#   PLAY_IMAGE=ghcr.io/ryancaudill04/minecraft-server/play:<tag>
#   ADMIN_IMAGE=ghcr.io/ryancaudill04/minecraft-server/admin:<tag>
#   MC_SERVER_IMAGE=ghcr.io/ryancaudill04/minecraft-server/mc-server:inputs-<hash>
#
# play and admin are built for every release. mc-server is tagged by a hash
# of everything that goes into its image instead, so a release that doesn't
# change any of it pins the same image, CI doesn't rebuild it, and the
# deploy doesn't restart the Minecraft server.
#
# IMAGE_REPO overrides where the images live (default
# ghcr.io/ryancaudill04/minecraft-server).
#
# Usage: scripts/images.sh <tag>
set -euo pipefail

tag=${1:?usage: images.sh <tag>}
[[ $tag =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]] || { echo "images.sh: bad tag $tag" >&2; exit 2; }
root=$(cd "$(dirname "$0")/.." && pwd)
repo=${IMAGE_REPO:-ghcr.io/ryancaudill04/minecraft-server}

if command -v sha256sum >/dev/null; then
  sha256() { sha256sum | cut -d' ' -f1; }
else # macOS
  sha256() { shasum -a 256 | cut -d' ' -f1; }
fi

# What mc-server's Dockerfile copies, or runs to fetch the server files.
# Each file's path and content count, so a rename changes the hash too.
inputs=(scripts/fetch-server.sh deploy/server.json)
while IFS= read -r f; do
  inputs+=("$f")
done < <(cd "$root" && find deploy/containers/mc-server -type f | LC_ALL=C sort)

hash=$(
  cd "$root"
  for f in "${inputs[@]}"; do
    [ -f "$f" ] || { echo "images.sh: $f is missing" >&2; exit 1; }
    printf '%s %s\n' "$(sha256 <"$f")" "$f"
  done | sha256
)

printf 'PLAY_IMAGE=%s/play:%s\n' "$repo" "$tag"
printf 'ADMIN_IMAGE=%s/admin:%s\n' "$repo" "$tag"
printf 'MC_SERVER_IMAGE=%s/mc-server:inputs-%s\n' "$repo" "${hash:0:16}"
