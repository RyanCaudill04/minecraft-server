#!/usr/bin/env bash
# Applies the repo's rulesets and protection settings with gh api. Run it
# by hand as the repo owner; CI never runs it. Each ruleset in
# .github/rulesets/*.json replaces the repo's ruleset of the same name, or
# is created if there is none. Running it again changes nothing.
#
# Also sets, from #7:
#   - the production environment deploys only from main
#   - workflows from every outside contributor wait for approval
#   - GITHUB_TOKEN is read-only by default and can't approve PRs
#   - squash merges only, and merged branches are deleted
#
# Usage: scripts/apply-rulesets.sh [--dry-run] [owner/repo]
#   --dry-run  print the API calls instead of making them
set -euo pipefail

dry=false
if [ "${1:-}" = --dry-run ]; then
  dry=true
  shift
fi
repo=${1:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}
root=$(cd "$(dirname "$0")/.." && pwd)

# api runs gh api, or prints it with --dry-run. Reads always run.
api() {
  if $dry; then
    echo "would run: gh api $*"
  else
    gh api "$@" >/dev/null
  fi
}

existing=$(gh api "repos/$repo/rulesets" --paginate --jq '.[] | "\(.id)\t\(.name)"')
for file in "$root"/.github/rulesets/*.json; do
  name=$(jq -er .name "$file")
  id=$(awk -F'\t' -v n="$name" '$2 == n { print $1; exit }' <<<"$existing")
  if [ -n "$id" ]; then
    echo "updating ruleset \"$name\" ($id) from ${file#"$root"/}"
    api -X PUT "repos/$repo/rulesets/$id" --input "$file"
  else
    echo "creating ruleset \"$name\" from ${file#"$root"/}"
    api -X POST "repos/$repo/rulesets" --input "$file"
  fi
done

echo "production environment: deploy only from main"
api -X PUT "repos/$repo/environments/production" --input - <<'EOF'
{"deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}
EOF
if ! gh api "repos/$repo/environments/production/deployment-branch-policies" \
  --jq '.branch_policies[] | select(.type == "branch") | .name' 2>/dev/null | grep -qx main; then
  api -X POST "repos/$repo/environments/production/deployment-branch-policies" -f name=main -f type=branch
fi

echo "actions: approval for outside contributors, read-only GITHUB_TOKEN"
api -X PUT "repos/$repo/actions/permissions/fork-pr-contributor-approval" \
  -f approval_policy=all_external_contributors
api -X PUT "repos/$repo/actions/permissions/workflow" \
  -f default_workflow_permissions=read -F can_approve_pull_request_reviews=false

echo "merges: squash only, delete merged branches"
api -X PATCH "repos/$repo" -F allow_squash_merge=true -F allow_merge_commit=false \
  -F allow_rebase_merge=false -F delete_branch_on_merge=true

echo "done"
