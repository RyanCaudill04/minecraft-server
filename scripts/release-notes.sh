#!/usr/bin/env bash
# Reads the changes in a release (one JSON object per line on stdin) and
# prints the release's notes.json.
#
# Each input line is {"number": 87 or null, "title": "...", "labels": [...]},
# where number is null for a commit pushed without a PR. A change is a
# feature if it's labeled enhancement, else a fix if it's labeled bug,
# else other. The release takes the highest kind of its changes, in that
# same order; a release with no changes is other.
#
# released_at is the current UTC time, or $RELEASED_AT if set (for tests).
#
# Usage: scripts/release-notes.sh <tag> < changes.jsonl > notes.json
set -euo pipefail

tag=${1:?usage: release-notes.sh <tag> < changes.jsonl}
at=${RELEASED_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}

jq -s --arg tag "$tag" --arg at "$at" '
  def kind:
    if any(.labels[]; . == "enhancement") then "feature"
    elif any(.labels[]; . == "bug") then "fix"
    else "other" end;
  map({pr: .number, title, kind: kind}) as $changes
  | ($changes | map(.kind)) as $kinds
  | {
      tag: $tag,
      kind: (first("feature", "fix" | select(IN($kinds[]))) // "other"),
      released_at: $at,
      changes: $changes
    }
'
