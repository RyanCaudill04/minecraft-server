#!/usr/bin/env bash
# Reads changed file paths (one per line) on stdin and prints which CI
# scopes they touch, as key=true|false lines for $GITHUB_OUTPUT.
#
#   go         always true: Go vet and unit tests are cheap and cached
#   play       play.ryancaudill.dev: its command, its site, the shared
#              stylesheet, or shared Go code
#   admin      admin.ryancaudill.dev and the other static sites: admin,
#              serve, their sites, the shared stylesheet, or shared Go code
#   mc_server  the Minecraft server's own files (configs, jars, start.sh)
#   infra      deploy files, scripts and workflows
#   deploy     anything that ships in a release, so main publishes one.
#              mc-players (a dev tool, #32) and deploy/server.json (read
#              on the server, not shipped) don't count.
#
# A change to .github/ turns every scope on.
#
# Paths must be unquoted, so turn off git's quoting of non-ASCII names.
#
# Usage: git -c core.quotePath=false diff --name-only A B | scripts/ci-changes.sh
set -euo pipefail

files=$(cat)

# git still quotes paths containing ", \ or control characters. Fail
# rather than silently match nothing.
if grep -q '^"' <<<"$files"; then
  echo "ci-changes.sh: quoted path in input, can't match it:" >&2
  grep '^"' <<<"$files" >&2
  exit 1
fi

touched() { grep -qE "$1" <<<"$files"; }
# Files under web/ or deploy/ that never go into a release.
shipped=$(grep -vE '^web/cmd/mc-players/|^deploy/server\.json$' <<<"$files" || true)

shared='^web/(internal/|go\.mod$|go\.sum$)|^sites/shared/'
play=false admin=false mc_server=false infra=false deploy=false

touched "$shared|^web/cmd/play/|^sites/play/" && play=true
touched "$shared|^web/cmd/(admin|serve)/|^sites/(admin|portfolio)/" && admin=true
touched '^(lazymc\.toml|start\.sh|server\.properties\.example|server\.jar|eula\.txt|bukkit\.yml|spigot\.yml|commands\.yml|ops\.json|whitelist\.json|banned-[a-z]+\.json)$|^(config|plugins|versions)/|^deploy/server\.json$' && mc_server=true
touched '^(deploy|scripts|\.github)/' && infra=true
grep -qE '^(web|sites|deploy)/|^scripts/build-release\.sh$|^\.github/workflows/' <<<"$shipped" && deploy=true

if touched '^\.github/'; then
  play=true admin=true mc_server=true infra=true
fi

printf 'go=true\nplay=%s\nadmin=%s\nmc_server=%s\ninfra=%s\ndeploy=%s\n' \
  "$play" "$admin" "$mc_server" "$infra" "$deploy"
