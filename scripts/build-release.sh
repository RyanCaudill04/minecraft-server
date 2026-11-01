#!/usr/bin/env bash
# Builds a release for the Pi: arm64 binaries, static sites and deploy
# files, packed with a SHA-256 checksum file.
#
#   <track>  one track from deploy/tracks.json (play, admin, sites, agent):
#            only that track's binaries, sites (each track has its own copy
#            of sites/shared) and units, as <track>-linux-arm64.tar.gz.
#            Its deploy/release.json names the track.
#   single   the single release that agents from before tracks install:
#            every track in one release.tar.gz, with units rewritten to run
#            from ~/apps/current instead of ~/apps/<track>/current.
#
# A notes file (from release-notes.sh) ships as deploy/notes.json.
#
# Usage: scripts/build-release.sh <track|single> <tag> <output folder> [notes.json]
set -euo pipefail

usage='usage: build-release.sh <track|single> <tag> <output folder> [notes.json]'
track=${1:?$usage}
tag=${2:?$usage}
out=${3:?$usage}
notes=${4:-}
root=$(cd "$(dirname "$0")/.." && pwd)
tracks="$root/deploy/tracks.json"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

if [ "$track" = single ]; then
  # Every track, in deploy/tracks.json's order, without duplicates.
  spec=$(jq -c '[.[]] | {
    bins: (map(.bins[]) | unique), sites: (map(.sites[]) | unique),
    units: [.[].units[]], services: [.[].services[]],
    timers: [.[].timers[]], health: [.[].health[]]}' "$tracks")
  manifest=$(jq '{units, services, timers, health}' <<<"$spec")
  archive=release.tar.gz
elif jq -e --arg t "$track" 'has($t)' "$tracks" >/dev/null; then
  spec=$(jq -c --arg t "$track" '.[$t]' "$tracks")
  manifest=$(jq --arg t "$track" '{track: $t, units, services, timers, health}' <<<"$spec")
  archive=$track-linux-arm64.tar.gz
else
  echo "build-release: no track $track in deploy/tracks.json" >&2
  exit 1
fi

mkdir -p "$stage/bin" "$stage/sites" "$stage/deploy/systemd" "$out"
(
  cd "$root/web"
  for cmd in $(jq -r '.bins[]' <<<"$spec"); do
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
      -ldflags "-s -w -X main.version=$tag" -o "$stage/bin/$cmd" "./cmd/$cmd"
  done
)
for site in $(jq -r '.sites[]' <<<"$spec"); do
  cp -R "$root/sites/$site" "$stage/sites/$site"
done
for unit in $(jq -r '.units[]' <<<"$spec"); do
  cp "$root/deploy/systemd/$unit" "$stage/deploy/systemd/$unit"
done
# Template instances read their settings from deploy/sites.
if jq -e '.units | any(test("@"))' <<<"$spec" >/dev/null; then
  cp -R "$root/deploy/sites" "$stage/deploy/sites"
fi
echo "$manifest" > "$stage/deploy/release.json"

if [ "$track" = single ]; then
  sed -i.bak -E 's#(%h|~)/apps/[a-z0-9]+/current#\1/apps/current#g' "$stage"/deploy/systemd/*
  rm -f "$stage"/deploy/systemd/*.bak
  bad='%h/apps/[a-z0-9]+/current'
else
  # A track's units may only use its own folder, or the shared ~/apps root.
  bad="%h/apps/($(jq -r --arg t "$track" 'keys[] | select(. != $t)' "$tracks" | paste -sd'|' -))/"
fi
if grep -nE "$bad" "$stage"/deploy/systemd/*; then
  echo "build-release: $track's units use another track's folder" >&2
  exit 1
fi

if [ -n "$notes" ]; then
  # Fail here, not on the Pi, if the notes aren't valid JSON.
  jq -e . "$notes" >/dev/null || { echo "build-release: $notes is not valid JSON" >&2; exit 1; }
  cp "$notes" "$stage/deploy/notes.json"
fi
echo "$tag" > "$stage/VERSION"

# Sorted names and fixed owners and times make the tarball reproducible.
# Pick the tar by its version, not by whether the GNU flags fail, so a real
# error (disk full, unreadable file) fails the build instead of falling back.
case "$(tar --version 2>/dev/null)" in
  *"GNU tar"*)
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='2000-01-01 00:00Z' \
      -C "$stage" -czf "$out/$archive" . ;;
  *) # BSD tar on macOS lacks those flags
    tar -C "$stage" -czf "$out/$archive" . ;;
esac
if command -v sha256sum >/dev/null; then
  (cd "$out" && sha256sum "$archive" > "$archive.sha256")
else # macOS
  (cd "$out" && shasum -a 256 "$archive" > "$archive.sha256")
fi

echo "built $tag ($archive) in $out"
