#!/usr/bin/env bash
# Builds a release for the Pi: arm64 binaries, the static sites, and the
# deploy files, packed as release.tar.gz plus a SHA-256 checksum file.
# A notes file (from release-notes.sh) ships as deploy/notes.json.
#
# Usage: scripts/build-release.sh <tag> <output folder> [notes.json]
set -euo pipefail

tag=${1:?usage: build-release.sh <tag> <output folder>}
out=${2:?usage: build-release.sh <tag> <output folder>}
notes=${3:-}
root=$(cd "$(dirname "$0")/.." && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

mkdir -p "$stage/bin" "$out"
(
  cd "$root/web"
  for cmd in play admin serve mc-agent ddns; do
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
      -ldflags "-s -w -X main.version=$tag" -o "$stage/bin/$cmd" "./cmd/$cmd"
  done
)

cp -R "$root/sites" "$stage/sites"
mkdir -p "$stage/deploy"
cp -R "$root/deploy/systemd" "$root/deploy/sites" "$root/deploy/release.json" "$stage/deploy/"
if [ -n "$notes" ]; then
  # Fail here, not on the Pi, if the notes aren't valid JSON.
  jq -e . "$notes" >/dev/null || { echo "build-release: $notes is not valid JSON" >&2; exit 1; }
  cp "$notes" "$stage/deploy/notes.json"
fi
echo "$tag" > "$stage/VERSION"

# Sorted names and fixed owners and times make the tarball reproducible.
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='2000-01-01 00:00Z' \
  -C "$stage" -czf "$out/release.tar.gz" . 2>/dev/null \
  || tar -C "$stage" -czf "$out/release.tar.gz" .   # BSD tar on macOS lacks those flags
if command -v sha256sum >/dev/null; then
  (cd "$out" && sha256sum release.tar.gz > release.tar.gz.sha256)
else # macOS
  (cd "$out" && shasum -a 256 release.tar.gz > release.tar.gz.sha256)
fi

echo "built $tag in $out"
