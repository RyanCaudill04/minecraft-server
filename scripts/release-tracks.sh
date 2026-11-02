#!/usr/bin/env bash
# Prints the tracks in deploy/tracks.json that need a new release at head,
# one "<track> <next tag> <last tag>" line each:
#
#   play play-r4 play-r3
#   admin admin-r1 -        (no release yet)
#
# Each track is compared with its own newest <track>-r<N> tag, so a track
# that didn't change isn't released, and a run that failed partway is
# caught up by the next one. A track whose newest tag isn't an ancestor of
# head (no release yet, or history was rewritten) is released. One whose
# newest tag already contains head (a re-run) is not.
#
# Needs the full history and tags (fetch-depth: 0).
#
# Usage: scripts/release-tracks.sh [head]
set -euo pipefail

head=${1:-HEAD}
dir=$(cd "$(dirname "$0")" && pwd)

for track in $(jq -r 'keys_unsorted[]' "$dir/../deploy/tracks.json"); do
  last=$(git tag -l "$track-r[0-9]*" | sed -n "s/^$track-r\([0-9][0-9]*\)\$/\1/p" | sort -n | tail -n1)
  next="$track-r$((${last:-0} + 1))"
  if [ -z "$last" ]; then
    echo "$track $next -"
    continue
  fi
  base="$track-r$last"
  if git merge-base --is-ancestor "$head" "$base"; then
    continue # already released
  fi
  if ! git merge-base --is-ancestor "$base" "$head"; then
    echo "$track $next -"
    continue
  fi
  # Captured first: grep -q in the pipe could exit before ci-changes.sh
  # is done writing, and pipefail would count the broken pipe as a no.
  scopes=$(git -c core.quotePath=false diff --name-only "$base" "$head" | "$dir/ci-changes.sh")
  if grep -qx "$track=true" <<<"$scopes"; then
    echo "$track $next $base"
  fi
done
