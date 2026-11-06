#!/usr/bin/env bash
# Prints the changes on main since a release, one JSON line per merged PR
# (or per commit pushed without one), newest first, ready for
# release-notes.sh. A merge of dev into main is listed as the PRs merged
# into dev that it brought in:
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

# pull_for <sha> <branch>: the merged PR into <branch> that <sha> came
# from, with its head branch, or nothing.
pull_for() {
  gh api "repos/{owner}/{repo}/commits/$1/pulls" --jq "
    [.[] | select(.merged_at != null and .base.ref == \"$2\")][0]
    // empty
    | {number, title, labels: [.labels[].name], head: .head.ref}"
}

# change <sha> <branch>: one line for the PR into <branch> that <sha> came
# from, or for the commit itself if it was pushed without one.
change() {
  local pr
  pr=$(pull_for "$1" "$2")
  if [ -n "$pr" ]; then
    jq -c '{number, title, labels}' <<<"$pr"
  else
    jq -nc --arg title "$(git log -1 --format=%s "$1")" '{number: null, title: $title, labels: []}'
  fi
}

# --first-parent keeps main's own commits (squash or merge commits), not
# the commits inside each PR's branch. Changes reach main by merging dev,
# so a merge of a PR from dev stands for the PRs that went into dev: list
# those instead, from dev's own commits that the merge brought in.
git log --first-parent --format=%H "$base..$head" | while read -r sha; do
  pr=$(pull_for "$sha" main)
  if [ -n "$pr" ] && [ "$(jq -r .head <<<"$pr")" = dev ] && git rev-parse -q --verify "$sha^2" >/dev/null; then
    git log --first-parent --format=%H "$sha^1..$sha^2" | while read -r dev_sha; do
      change "$dev_sha" dev
    done
  elif [ -n "$pr" ]; then
    jq -c '{number, title, labels}' <<<"$pr"
  else
    change "$sha" main
  fi
done | jq -c -s '
  # A PR whose commits all landed on main appears once per commit; keep
  # its first (newest) line.
  reduce .[] as $c ([];
    if $c.number != null and any(.[]; .number == $c.number) then . else . + [$c] end)
  | .[]'
