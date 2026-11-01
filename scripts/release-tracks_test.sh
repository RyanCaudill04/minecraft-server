#!/usr/bin/env bash
# Tests for release-tracks.sh, in a throwaway git repo.
#
# Usage: scripts/release-tracks_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
repo=$(mktemp -d)
trap 'rm -rf "$repo"' EXIT
failures=0

mkdir -p "$repo/scripts" "$repo/deploy"
cp "$dir/release-tracks.sh" "$dir/ci-changes.sh" "$repo/scripts/"
cp "$dir/../deploy/tracks.json" "$repo/deploy/"
cd "$repo"
git init -q
git config user.email test@example.com
git config user.name test
git config commit.gpgsign false
git config tag.gpgsign false

commit() { # commit <file>...: change each file and commit
  for f in "$@"; do
    mkdir -p "$(dirname "$f")"
    echo "$RANDOM" >>"$f"
  done
  git add -A
  git commit -qm "change $*"
}

check() { # check <name> <want lines, ; separated> [head]
  local got want
  got=$(scripts/release-tracks.sh "${3:-HEAD}" | paste -sd';' -)
  want=$2
  if [ "$got" != "$want" ]; then
    echo "FAIL $1: got '$got', want '$want'"
    failures=$((failures + 1))
  fi
}

commit web/cmd/play/main.go web/cmd/admin/main.go web/cmd/serve/main.go web/cmd/mc-agent/main.go
check "no releases yet" 'play play-r1 -;admin admin-r1 -;sites sites-r1 -;agent agent-r1 -'

for t in play admin sites agent; do git tag "$t-r1"; done
check "everything released" ''

commit sites/play/app.js
check "play site only" 'play play-r2 play-r1'
git tag play-r2
check "re-run after release" ''
check "older commit" '' HEAD~1

commit web/internal/site/site.go
check "shared package" 'play play-r3 play-r2;admin admin-r2 admin-r1'

commit web/internal/deploy/deploy.go
check "deploy package" 'play play-r3 play-r2;admin admin-r2 admin-r1;agent agent-r2 agent-r1'

commit README.md
git tag play-r3 && git tag admin-r2 && git tag agent-r2
check "readme only" ''

# Tag numbers sort as numbers, not text.
git tag sites-r9 && git tag sites-r10 && git tag sites-r10x
commit sites/portfolio/index.html
check "number order" 'sites sites-r11 sites-r10'

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "release-tracks: all cases pass"
