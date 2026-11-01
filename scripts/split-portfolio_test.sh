#!/usr/bin/env bash
# Tests for split-portfolio.sh against a made-up repo: the dry run changes
# nothing, and a real run keeps only the portfolio's commits and files,
# with their authors and dates, then pushes only when asked.
#
# Usage: scripts/split-portfolio_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failures=0
fail() { echo "FAIL $*"; failures=$((failures + 1)); }

export GIT_CONFIG_GLOBAL=$tmp/gitconfig GIT_CONFIG_NOSYSTEM=1
git config --global user.name "Owner"
git config --global user.email owner@example.com
git config --global init.defaultBranch main

commit() { # author, date, message, then files to write
  local who=$1 date=$2 msg=$3 f
  shift 3
  for f in "$@"; do
    mkdir -p "$(dirname "$f")"
    printf '<!doctype html><title>%s</title>\n' "$f $date" > "$f"
  done
  git add -A
  GIT_AUTHOR_NAME=$who GIT_AUTHOR_EMAIL=$who@example.com GIT_AUTHOR_DATE=$date \
    GIT_COMMITTER_NAME=$who GIT_COMMITTER_EMAIL=$who@example.com GIT_COMMITTER_DATE=$date \
    git commit -q -m "$msg"
}

src=$tmp/src
git init -q "$src"
(
  cd "$src"
  commit Alice 2026-01-01T10:00:00Z $'Add the sites\n\nBody.\n\nCo-Authored-By: Bot <bot@example.com>' \
    sites/portfolio/index.html sites/play/index.html web/main.go
  commit Bob 2026-01-02T10:00:00Z 'Change play' sites/play/index.html
  commit Carol 2026-01-03T10:00:00Z 'Add a portfolio page' sites/portfolio/about/index.html README.md
  git tag r1
)

# Dry run: prints the plan and creates nothing.
out=$tmp/out
got=$("$dir/split-portfolio.sh" --source "$src" --out "$out" 2>&1) || fail "dry run exited non-zero: $got"
[ -e "$out" ] && fail "dry run created $out"
grep -q 'Dry run' <<<"$got" || fail "dry run doesn't say so: $got"
for who in Alice Carol; do
  grep -q "$who" <<<"$got" || fail "dry run doesn't list $who's portfolio commit: $got"
done
grep -q 'Bob' <<<"$got" && fail "dry run lists a commit that doesn't touch the portfolio"
grep -qx '  site/about/index.html' <<<"$got" || fail "dry run doesn't list site/about/index.html: $got"
grep -qx '  README.md' <<<"$got" || fail "dry run doesn't list the template README: $got"

# Bad arguments.
"$dir/split-portfolio.sh" --source "$src" --out "$out" --push "$tmp/x.git" >/dev/null 2>&1 \
  && fail "--push without --run should fail"
mkdir -p "$tmp/full" && touch "$tmp/full/x"
"$dir/split-portfolio.sh" --run --source "$src" --out "$tmp/full" >/dev/null 2>&1 \
  && fail "a non-empty --out should fail"
"$dir/split-portfolio.sh" --source "$src" --out "$out" --drop-lines '(' >/dev/null 2>&1 \
  && fail "a bad --drop-lines regex should fail"

# Real run, without pushing.
"$dir/split-portfolio.sh" --run --source "$src" --out "$out" --drop-lines '^Co-Authored-By: Bot' >/dev/null \
  || fail "real run exited non-zero"
cd "$out"
want=$(cd "$dir/portfolio-repo" && { find . -type f | sed 's|^\./||'; echo site/index.html; echo site/about/index.html; } | sort)
[ "$(git ls-files | sort)" = "$want" ] || fail "files: got $(git ls-files | tr '\n' ' ')"
[ "$(git log --reverse --format='%an <%ae> %aI|%cn %cI' | head -2)" = \
  "Alice <Alice@example.com> 2026-01-01T10:00:00+00:00|Alice 2026-01-01T10:00:00+00:00
Carol <Carol@example.com> 2026-01-03T10:00:00+00:00|Carol 2026-01-03T10:00:00+00:00" ] \
  || fail "authors or dates changed: $(git log --reverse --format='%an <%ae> %aI|%cn %cI')"
[ "$(git log -1 --format='%an <%ae>')" = "Owner <owner@example.com>" ] || fail "setup commit isn't by the configured user"
[ "$(git rev-list --count HEAD)" = 3 ] || fail "want 3 commits, got $(git rev-list --count HEAD)"
[ "$(git branch -a --format='%(refname)')" = refs/heads/main ] || fail "want only main, got $(git branch -a)"
[ -z "$(git tag)" ] || fail "tags came along: $(git tag)"
[ -z "$(git remote)" ] || fail "remotes left: $(git remote)"
git log --format=%B | grep -q 'Co-Authored-By: Bot' && fail "--drop-lines didn't drop the trailer"
git log --format=%B | grep -qx 'Body.' || fail "the rest of the message was lost"
[ "$(git log --format=%B --reverse | head -3)" = $'Add the sites\n\nBody.' ] || fail "message: $(git log --format=%B --reverse | head -4)"
git status --porcelain | grep -q . && fail "working tree isn't clean"
scripts/check.sh >/dev/null || fail "check.sh fails on the new repo"
scripts/build-release.sh t1 "$tmp/dist" >/dev/null || fail "build-release.sh fails on the new repo"
tar -tzf "$tmp/dist/portfolio.tar.gz" | grep -qx './site/about/index.html' || fail "release is missing site/about/index.html"
cd "$tmp"

# The source repo is untouched.
[ "$(git -C "$src" log --format=%an | tr '\n' ' ')" = "Carol Bob Alice " ] || fail "source history changed"
[ -z "$(git -C "$src" status --porcelain)" ] || fail "source working tree changed"

# Real run with --push to a local bare repo.
git init -q --bare "$tmp/remote.git"
"$dir/split-portfolio.sh" --run --source "$src" --out "$tmp/out2" --push "$tmp/remote.git" >/dev/null 2>&1 \
  || fail "run with --push exited non-zero"
[ "$(git -C "$tmp/remote.git" rev-parse main 2>/dev/null)" = "$(git -C "$tmp/out2" rev-parse main)" ] \
  || fail "--push didn't push main"

# Without --drop-lines, messages are kept as they are.
"$dir/split-portfolio.sh" --run --source "$src" --out "$tmp/out3" >/dev/null
git -C "$tmp/out3" log --format=%B | grep -q 'Co-Authored-By: Bot' || fail "message changed without --drop-lines"

# Splitting another branch still gives a repo with only main.
git -C "$src" branch -q other
"$dir/split-portfolio.sh" --run --source "$src" --branch other --out "$tmp/out4" >/dev/null
[ "$(git -C "$tmp/out4" branch -a --format='%(refname)')" = refs/heads/main ] \
  || fail "--branch: want only main, got $(git -C "$tmp/out4" branch -a)"
[ "$(git -C "$tmp/out4" rev-list --count main)" = 3 ] || fail "--branch: want 3 commits"

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "split-portfolio: all cases pass"
