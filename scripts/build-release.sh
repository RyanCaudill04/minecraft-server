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
mkdir -p "$stage/deploy"
cp -R "$root/deploy/systemd" "$root/deploy/sites" "$root/deploy/release.json" "$stage/deploy/"
if [ -n "$notes" ]; then
  # Fail here, not on the Pi, if the notes aren't valid JSON.
  jq -e . "$notes" >/dev/null || { echo "build-release: $notes is not valid JSON" >&2; exit 1; }
  cp "$notes" "$stage/deploy/notes.json"
fi
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
    for cmd in play admin serve mc-agent; do
      CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X main.version=$tag" -o "$stage/bin/$cmd" "./cmd/$cmd"
    done
  )

  name=release-linux-$arch.tar.gz
  # Sorted names and fixed owners and times make the tarball reproducible.
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='2000-01-01 00:00Z' \
    -C "$stage" -czf "$out/$name" . 2>/dev/null \
    || tar -C "$stage" -czf "$out/$name" .   # BSD tar on macOS lacks those flags
  checksum "$name"
done

# Agents from before per-architecture tarballs only know this name. Drop
# it once the Pi runs an agent that picks its tarball by architecture.
cp "$out/release-linux-arm64.tar.gz" "$out/release.tar.gz"
checksum release.tar.gz

echo "built $tag in $out"
