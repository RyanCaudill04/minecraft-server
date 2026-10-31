#!/usr/bin/env bash
# Prints the changes on main since a release, one JSON line per merged PR
# (or per commit pushed without one), newest first, ready for
# release-notes.sh:
#
#   {"number": 87 or null, "title": "...", "labels": ["bug", ...]}
#
# base defaults to the newest r<N> tag. If it isn't an ancestor of head
# (no release yet, or history was rewritten), nothing is printed.
#
# Needs the full history and tags (fetch-depth: 0) and gh with a token
# that can read pull requests.
#
# Usage: scripts/release-changes.sh [base [head]]
set -euo pipefail

base=${1:-$(git tag -l 'r[0-9]*' --sort=-v:refname | grep -Ex -m1 'r[0-9]+' || true)}
head=${2:-HEAD}

if [ -z "$base" ] || ! git merge-base --is-ancestor "$base" "$head"; then
  echo "release-changes: no release before $head to compare with" >&2
  exit 0
fi

# --first-parent keeps main's own commits (squash or merge commits), not
# the commits inside each PR's branch.
git log --first-parent --format=%H "$base..$head" | while read -r sha; do
  pr=$(gh api "repos/{owner}/{repo}/commits/$sha/pulls" --jq '
    [.[] | select(.merged_at != null and .base.ref == "main")][0]
    // empty
    | {number, title, labels: [.labels[].name]}')
  if [ -n "$pr" ]; then
    echo "$pr" | jq -c .
  else
    jq -nc --arg title "$(git log -1 --format=%s "$sha")" '{number: null, title: $title, labels: []}'
  fi
done | jq -c -s '
  # A PR whose commits all landed on main appears once per commit; keep
  # its first (newest) line.
  reduce .[] as $c ([];
    if $c.number != null and any(.[]; .number == $c.number) then . else . + [$c] end)
  | .[]'
