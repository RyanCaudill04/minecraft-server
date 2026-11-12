#!/usr/bin/env bash
# Reads changed file paths (one per line) on stdin and prints which CI
# scopes they touch, as key=true|false lines for $GITHUB_OUTPUT.
#
#   go         always true: Go vet and unit tests are cheap and cached
#   play       play.ryancaudill.dev: its command, its site, its tests in
#              tests/play, the shared stylesheet, or shared Go code
#   admin      admin.ryancaudill.dev: its command, its site, its tests in
#              tests/admin, the shared stylesheet, or shared Go code
#   mc_server  the Minecraft server's own files (configs, jars, and its
#              container's start.sh and lazymc.toml), plus fetch-server.sh
#              and mc-players, which its checks run
#   infra      deploy files, scripts and workflows, and the dev sites on
#              Cloudflare (dev/, tests/dev), which never go into a release
#   images     anything that goes into the play, admin or mc-server images
#              (deploy/containers), so they're built and smoke-tested
#   deploy     anything that ships in a release or its images, so main
#              publishes one. mc-players (a dev tool, #32) doesn't count.
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
shipped=$(grep -vE '^web/cmd/mc-players/' <<<"$files" || true)

shared='^web/(internal/|go\.mod$|go\.sum$)|^sites/shared/'
play=false admin=false mc_server=false infra=false images=false deploy=false

touched "$shared|^web/cmd/play/|^sites/play/|^tests/play/" && play=true
touched "$shared|^web/cmd/admin/|^sites/admin/|^tests/admin/" && admin=true
touched '^(server\.properties\.example|server\.jar|eula\.txt|bukkit\.yml|spigot\.yml|commands\.yml|ops\.json|whitelist\.json|banned-[a-z]+\.json)$|^(config|plugins|versions)/|^deploy/containers/mc-server/|^deploy/server\.json$|^scripts/fetch-server\.sh$|^web/cmd/mc-players/' && mc_server=true
touched '^(deploy|scripts|dev|\.github)/|^tests/dev/' && infra=true
grep -qE '^(web|sites)/|^deploy/(containers/|server\.json$)|^scripts/(fetch-server|images)\.sh$|^\.dockerignore$' <<<"$shipped" && images=true
grep -qE '^(web|sites|deploy)/|^scripts/(build-release|images|fetch-server)\.sh$|^\.dockerignore$|^\.github/workflows/' <<<"$shipped" && deploy=true

if touched '^\.github/'; then
  play=true admin=true mc_server=true infra=true images=true
fi

printf 'play=%s\nadmin=%s\nmc_server=%s\ninfra=%s\nimages=%s\ndeploy=%s\n' \
  "$play" "$admin" "$mc_server" "$infra" "$images" "$deploy"
