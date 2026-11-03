#!/usr/bin/env bash
# Builds a release: site/ and a VERSION file, packed as portfolio.tar.gz
# plus a SHA-256 checksum file.
#
# Usage: scripts/build-release.sh <tag> <output folder>
set -euo pipefail

tag=${1:?usage: build-release.sh <tag> <output folder>}
out=${2:?usage: build-release.sh <tag> <output folder>}
root=$(cd "$(dirname "$0")/.." && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

mkdir -p "$out"
cp -R "$root/site" "$stage/site"
echo "$tag" > "$stage/VERSION"

# Sorted names and fixed owners and times make the tarball reproducible.
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='2000-01-01 00:00Z' \
  -C "$stage" -czf "$out/portfolio.tar.gz" . 2>/dev/null \
  || tar -C "$stage" -czf "$out/portfolio.tar.gz" .   # BSD tar on macOS lacks those flags
if command -v sha256sum >/dev/null; then
  (cd "$out" && sha256sum portfolio.tar.gz > portfolio.tar.gz.sha256)
else # macOS
  (cd "$out" && shasum -a 256 portfolio.tar.gz > portfolio.tar.gz.sha256)
fi

echo "built $tag in $out"
