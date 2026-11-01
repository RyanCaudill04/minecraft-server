#!/usr/bin/env bash
# Makes a standalone repo for the portfolio (#29): a fresh clone of this
# repo rewritten by git filter-repo so it keeps only sites/portfolio, moved
# to site/, with each commit's author, committer and dates unchanged. Then
# one commit on top adds the new repo's own files from scripts/portfolio-repo.
#
# Without --run it only prints what it would do and changes nothing. It
# pushes only with --push <url>, and never force-pushes. This repo and its
# checkout are never modified: the rewrite happens in the new folder.
#
# Needs git filter-repo (brew install git-filter-repo, or
# pipx install git-filter-repo).
#
# Usage: scripts/split-portfolio.sh [--run] [--out DIR] [--push URL]
#          [--source REPO] [--branch NAME] [--drop-lines REGEX]
#
#   --run          do it; without this it's a dry run
#   --out DIR      folder for the new repo, must not exist or be empty
#                  (default: a portfolio folder next to this repo)
#   --push URL     after --run, add URL as origin and push main to it
#   --source REPO  path or URL to split (default: this repo)
#   --branch NAME  branch of the source to split (default: main)
#   --drop-lines REGEX
#                  drop commit-message lines matching this Python regex,
#                  such as trailers you don't want carried over
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
templates="$root/scripts/portfolio-repo"
prefix=sites/portfolio
run=false
out="$(dirname "$root")/portfolio"
push=
source=$root
branch=main
drop=

usage() { sed -n '/^# Usage:/,/^set -/p' "$0" | sed '$d; s/^# \{0,1\}//' >&2; exit 2; }
die() { echo "split-portfolio: $*" >&2; exit 1; }

while (($#)); do
  case $1 in
    --run) run=true ;;
    --out) out=${2:?--out needs a folder}; shift ;;
    --push) push=${2:?--push needs a URL}; shift ;;
    --source) source=${2:?--source needs a repo}; shift ;;
    --branch) branch=${2:?--branch needs a name}; shift ;;
    --drop-lines) drop=${2:?--drop-lines needs a regex}; shift ;;
    -h|--help) usage ;;
    *) echo "split-portfolio: unknown argument $1" >&2; usage ;;
  esac
  shift
done

[ -n "$push" ] && ! $run && die "--push needs --run"
if [ -e "$out" ] && [ -n "$(ls -A "$out" 2>/dev/null)" ]; then
  die "$out already exists and isn't empty"
fi
[ -d "$templates" ] || die "missing $templates"
git filter-repo --version >/dev/null 2>&1 \
  || die "git filter-repo is not installed (brew install git-filter-repo)"
if [ -n "$drop" ]; then
  python3 -c 'import re, sys; re.compile(sys.argv[1])' "$drop" 2>/dev/null \
    || die "--drop-lines is not a valid regex: $drop"
fi

if ! $run; then
  # Read the source without changing it. A URL is cloned bare into a
  # temporary folder first.
  repo=$source
  if [ ! -d "$source" ]; then
    repo=$(mktemp -d)
    trap 'rm -rf "$repo"' EXIT
    git clone -q --bare --no-tags --single-branch --branch "$branch" "$source" "$repo"
  fi
  git -C "$repo" rev-parse -q --verify "$branch^{commit}" >/dev/null \
    || die "$source has no branch $branch"
  echo "Dry run. Nothing is created or pushed; add --run to do it."
  echo
  echo "Source:  $source ($branch at $(git -C "$repo" rev-parse --short "$branch"))"
  echo "New repo: $out"
  echo "Push to:  ${push:-nowhere (add --push URL)}"
  echo
  echo "Commits kept (author, date, subject), oldest first:"
  git -C "$repo" log --reverse --format='  %h %an <%ae> %ad %s' --date=short "$branch" -- "$prefix"
  echo "Then one new commit by $(git config user.name || true) <$(git config user.email || true)> adding the repo's own files."
  echo
  echo "Files in the new repo:"
  { git -C "$repo" ls-tree -r --name-only "$branch" -- "$prefix" | sed "s|^$prefix/|site/|"
    (cd "$templates" && find . -type f | sed 's|^\./||')
  } | sort | sed 's/^/  /'
  if [ -n "$drop" ]; then
    echo
    echo "Message lines that would be dropped (matching $drop):"
    git -C "$repo" log --format=%B "$branch" -- "$prefix" \
      | python3 -c 'import re, sys
pat = re.compile(sys.argv[1])
for line in sys.stdin:
    if pat.search(line):
        print("  " + line.rstrip())' "$drop"
  fi
  exit 0
fi

# Clone without hardlinks or tags (release tags belong to this repo), and
# only the one branch, so filter-repo sees a fresh clone it may rewrite.
git clone -q --no-local --no-tags --single-branch --branch "$branch" "$source" "$out"
cd "$out"
[ "$branch" = main ] || git branch -q -m "$branch" main
# Drop the source as a remote now: filter-repo would turn its branches
# into local ones.
git remote remove origin

args=(--path "$prefix/" --path-rename "$prefix/:site/")
if [ -n "$drop" ]; then
  # filter-repo runs this as the body of a function given the message as
  # bytes. The regex comes in through the environment, not the code.
  export SPLIT_DROP_LINES=$drop
  args+=(--message-callback '
import os, re
pat = re.compile(os.environ["SPLIT_DROP_LINES"].encode())
lines = [l for l in message.split(b"\n") if not pat.search(l)]
return b"\n".join(lines).rstrip(b"\n") + b"\n"
')
fi
# --force: filter-repo refuses a clone whose branch was renamed, but this
# folder was cloned just above and holds nothing else.
git filter-repo --quiet --force "${args[@]}"

[ -n "$(git ls-files)" ] || die "no files under $prefix on $branch"

cp -R "$templates/." .
git add -A
git commit -q -m "Set up the portfolio repo" \
  -m "Adds the README, CI checks and release build. The site's history comes from RyanCaudill04/minecraft-server (#29)."

echo "Created $out:"
git log --format='  %h %an <%ae> %ad %s' --date=short
echo
git ls-files | sed 's/^/  /'

if [ -n "$push" ]; then
  git remote add origin "$push"
  git push -u origin main
else
  echo
  echo "Not pushed. To push: git -C $out remote add origin <url> && git -C $out push -u origin main"
fi
