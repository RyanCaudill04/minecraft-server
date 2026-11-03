#!/usr/bin/env bash
# Builds a release: the binaries, the static sites, and the deploy files,
# packed once per architecture as release-linux-<arch>.tar.gz, each with a
# SHA-256 checksum file. Only the binaries differ between them.
# A notes file (from release-notes.sh) ships as deploy/notes.json.
#
# The arm64 tarball is also written as release.tar.gz, the only name that
# agents built before per-architecture tarballs download.
#
# Usage: scripts/build-release.sh <tag> <output folder> [notes.json]
set -euo pipefail

tag=${1:?usage: build-release.sh <tag> <output folder>}
out=${2:?usage: build-release.sh <tag> <output folder>}
notes=${3:-}
root=$(cd "$(dirname "$0")/.." && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

mkdir -p "$out"
cp -R "$root/sites" "$stage/sites"
mkdir -p "$stage/deploy/systemd"
cp -R "$root/deploy/sites" "$root/deploy/release.json" "$stage/deploy/"
# Only the units the manifest links. The others in deploy/systemd, like
# minecraft.service, are linked from the server checkout instead.
jq -r '.units[]' "$root/deploy/release.json" | while read -r unit; do
  cp "$root/deploy/systemd/$unit" "$stage/deploy/systemd/"
done
if [ -n "$notes" ]; then
  # Fail here, not on the Pi, if the notes aren't valid JSON.
  jq -e . "$notes" >/dev/null || { echo "build-release: $notes is not valid JSON" >&2; exit 1; }
  cp "$notes" "$stage/deploy/notes.json"
fi
# Nothing reads VERSION (mc-agent takes the tag from the current
# symlink); it's there for a human looking inside an unpacked release.
echo "$tag" > "$stage/VERSION"

checksum() {
  if command -v sha256sum >/dev/null; then
    (cd "$out" && sha256sum "$1" > "$1.sha256")
  else # macOS
    (cd "$out" && shasum -a 256 "$1" > "$1.sha256")
  fi
}

for arch in arm64 amd64; do
  rm -rf "${stage:?}/bin"
  mkdir -p "$stage/bin"
  (
    cd "$root/web"
    for cmd in play admin serve mc-agent mc-rcon; do
      CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X main.version=$tag" -o "$stage/bin/$cmd" "./cmd/$cmd"
    done
  )

  name=release-linux-$arch.tar.gz
  # Sorted names and fixed owners and times make the tarball reproducible.
  # Pick the tar flavor up front rather than falling back on any error, so
  # a real GNU tar failure (disk full, unreadable file) stops the build.
  if tar --version 2>/dev/null | grep -q GNU; then
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='2000-01-01 00:00Z' \
      -C "$stage" -czf "$out/$name" .
  else # BSD tar on macOS lacks those flags, so its tarball isn't reproducible
    tar -C "$stage" -czf "$out/$name" .
  fi
  checksum "$name"
done

# Agents from before per-architecture tarballs only know this name. Drop
# it once the Pi runs an agent that picks its tarball by architecture.
cp "$out/release-linux-arm64.tar.gz" "$out/release.tar.gz"
checksum release.tar.gz

echo "built $tag in $out"
