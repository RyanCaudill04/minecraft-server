#!/usr/bin/env bash
# Tests for release-changes.sh against a throwaway repo, with a fake gh
# that answers "which PRs is this commit in" from files. The fake still
# runs the script's own --jq filter, so the PR selection is tested too.
#
# Usage: scripts/release-changes_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0

# Fake gh: `gh api repos/{owner}/{repo}/commits/<sha>/pulls --jq <filter>`
# prints pulls/<sha>.json (or []) through the filter.
mkdir -p "$work/bin" "$work/pulls"
cat > "$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = api ] || { echo "fake gh: unexpected $*" >&2; exit 1; }
sha=$(sed -E 's#.*/commits/([0-9a-f]+)/pulls#\1#' <<<"$2")
file="$FAKE_PULLS/$sha.json"
[ -f "$file" ] || file=/dev/null
{ cat "$file"; [ -s "$file" ] || echo '[]'; } | jq "$4"
EOF
chmod +x "$work/bin/gh"
export PATH="$work/bin:$PATH" FAKE_PULLS="$work/pulls"

# pr <sha> <number> <title> <base> <head> [label...]: say <sha> came from
# that merged PR.
pr() {
  local sha=$1 number=$2 title=$3 base=$4 head=$5
  shift 5
  jq -n --argjson n "$number" --arg t "$title" --arg b "$base" --arg h "$head" \
    --args '[{number: $n, title: $t, merged_at: "2026-11-01T00:00:00Z",
              base: {ref: $b}, head: {ref: $h}, labels: [$ARGS.positional[] | {name: .}]}]' \
    "$@" > "$FAKE_PULLS/$sha.json"
}

repo=$work/repo
git init -q -b main "$repo"
cd "$repo"
git config user.email test@example.com
git config user.name Test
# commit <message>: a commit touching its own file, so merges never conflict.
commit() { echo "$1" > "$(tr -c "a-z0-9\n" - <<<"$1")"; git add -A; git commit -q -m "$1"; git rev-parse HEAD; }

commit "initial" >/dev/null
git tag r1

# Two PRs into dev: one squashed, one merged with a merge commit.
git switch -q -c dev
a=$(commit "play: add a feature")
pr "$a" 10 "Add a feature" dev feature-a enhancement play
git switch -q -c fix-b
commit "admin: fix a bug" >/dev/null
git switch -q dev
git merge -q --no-ff -m "Merge pull request #11 from fix-b" fix-b
b=$(git rev-parse HEAD)
pr "$b" 11 "Fix a bug" dev fix-b bug admin

# dev into main, then a PR straight into main and a push without a PR.
git switch -q main
git merge -q --no-ff -m "Merge pull request #12 from dev" dev
m=$(git rev-parse HEAD)
pr "$m" 12 "Release dev" main dev
s=$(commit "ci: squashed into main")
pr "$s" 13 "A PR into main" main feature-c ci-cd
commit "direct push" >/dev/null

got=$("$dir/release-changes.sh" r1 HEAD)
want='{"number":null,"title":"direct push","labels":[]}
{"number":13,"title":"A PR into main","labels":["ci-cd"]}
{"number":11,"title":"Fix a bug","labels":["bug","admin"]}
{"number":10,"title":"Add a feature","labels":["enhancement","play"]}'
if [ "$got" != "$want" ]; then
  echo "FAIL dev merge expands into its PRs"
  diff <(echo "$want") <(echo "$got") || true
  failures=$((failures + 1))
fi

# A second merge of dev lists only what came in since the first.
git switch -q dev
c=$(commit "play: a second feature")
pr "$c" 14 "A second feature" dev feature-d enhancement
git switch -q main
git merge -q --no-ff -m "Merge pull request #15 from dev" dev
m2=$(git rev-parse HEAD)
pr "$m2" 15 "Release dev again" main dev
git tag r2 "$m"
got=$("$dir/release-changes.sh" r2 "$m2")
want='{"number":14,"title":"A second feature","labels":["enhancement"]}
{"number":null,"title":"direct push","labels":[]}
{"number":13,"title":"A PR into main","labels":["ci-cd"]}'
if [ "$got" != "$want" ]; then
  echo "FAIL second dev merge lists only new PRs"
  diff <(echo "$want") <(echo "$got") || true
  failures=$((failures + 1))
fi

if [ "$failures" -gt 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "release-changes: all cases pass"
