#!/usr/bin/env bash
# Tests for release-notes.sh. Each case gives the input lines and a jq
# condition the notes must meet.
#
# Usage: scripts/release-notes_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
failures=0

check() {
  local name=$1 input=$2 want=$3 got
  got=$(printf '%s' "$input" | RELEASED_AT=2026-10-31T18:00:00Z "$dir/release-notes.sh" r7)
  if ! jq -e "$want" <<<"$got" >/dev/null; then
    echo "FAIL $name: want $want, got: $(jq -c . <<<"$got")"
    failures=$((failures + 1))
  fi
}

feature='{"number":1,"title":"a","labels":["enhancement"]}'
fix='{"number":2,"title":"b","labels":["bug"]}'
both='{"number":3,"title":"c","labels":["bug","enhancement"]}'
unlabeled='{"number":4,"title":"d","labels":["documentation"]}'
push='{"number":null,"title":"e","labels":[]}'

check "nothing"        ''                         '.kind == "other" and .changes == []'
check "fields"         "$fix"                     '.tag == "r7" and .released_at == "2026-10-31T18:00:00Z"'
check "fix only"       "$fix"                     '.kind == "fix" and .changes == [{"pr":2,"title":"b","kind":"fix"}]'
check "feature only"   "$feature"                 '.kind == "feature" and .changes[0].kind == "feature"'
check "both labels"    "$both"                    '.kind == "feature" and .changes[0].kind == "feature"'
check "other labels"   "$unlabeled"               '.kind == "other" and .changes[0].kind == "other"'
check "direct push"    "$push"                    '.kind == "other" and .changes == [{"pr":null,"title":"e","kind":"other"}]'
check "fix and other"  "$fix"$'\n'"$unlabeled"    '.kind == "fix" and (.changes | map(.kind)) == ["fix","other"]'
check "mixed"          "$unlabeled"$'\n'"$fix"$'\n'"$feature" '.kind == "feature" and (.changes | map(.pr)) == [4,2,1]'
check "trailing line"  "$fix"$'\n'                '.changes | length == 1'
# shellcheck disable=SC2016 # the $ and backticks are meant literally
check "odd title"      '{"number":5,"title":"Say \"hi\" to $HOME `x`","labels":[]}' \
  '.changes[0].title == "Say \"hi\" to $HOME `x`"'

if "$dir/release-notes.sh" </dev/null >/dev/null 2>&1; then
  echo "FAIL no tag: want a non-zero exit"
  failures=$((failures + 1))
fi

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "release-notes: all cases pass"
