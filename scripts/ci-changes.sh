#!/usr/bin/env bash
# Reads changed file paths (one per line) on stdin and prints which CI
# scopes they touch, as key=true|false lines for $GITHUB_OUTPUT.
#
#   go         always true: Go vet and unit tests are cheap and cached
#   play       the play track: its command, its site, the shared
#              stylesheet, its unit, or Go code it imports
#   admin      the admin track, likewise
#   sites      the sites track: serve, the portfolio, deploy/sites and
#              site@.service
#   agent      the agent track: mc-agent, its units, or Go code it imports
#   mc_server  the Minecraft server's own files (configs, jars, start.sh)
#   infra      deploy files, scripts and workflows
#   deploy     anything that ships in a release, so main publishes one
#
# Each track output means the track's release would change, so main
# publishes a new release of it. Files every release is built from
# (deploy/tracks.json, build-release.sh, go.mod) turn on every track, and
# a change to .github/ turns every scope on.
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

# The web/internal packages each track's binary imports (go list -deps).
# ci-changes_test.sh checks these against the code.
play_pkgs='mcstatus|nbt|ping|site|world'
admin_pkgs='deploy|mcstatus|ping|site'
sites_pkgs=''
agent_pkgs='deploy'

every='^web/(go\.mod|go\.sum)$|^deploy/tracks\.json$|^scripts/build-release\.sh$'
pkgs() { [ -n "$1" ] && echo "|^web/internal/($1)/" || true; }

play=false admin=false sites=false agent=false mc_server=false infra=false deploy=false

touched "$every$(pkgs "$play_pkgs")|^web/cmd/play/|^sites/(play|shared)/|^deploy/systemd/play\.service$" && play=true
touched "$every$(pkgs "$admin_pkgs")|^web/cmd/admin/|^sites/(admin|shared)/|^deploy/systemd/admin\.service$" && admin=true
touched "$every$(pkgs "$sites_pkgs")|^web/cmd/serve/|^sites/portfolio/|^deploy/sites/|^deploy/systemd/site@\.service$" && sites=true
touched "$every$(pkgs "$agent_pkgs")|^web/cmd/mc-agent/|^deploy/systemd/mc-agent\.(service|timer)$" && agent=true
touched '^(lazymc\.toml|start\.sh|server\.properties\.example|server\.jar|eula\.txt|bukkit\.yml|spigot\.yml|commands\.yml|ops\.json|whitelist\.json|banned-[a-z]+\.json)$|^(config|plugins|versions)/' && mc_server=true
touched '^(deploy|scripts|\.github)/' && infra=true
touched '^(web|sites|deploy)/|^scripts/build-release\.sh$|^\.github/workflows/' && deploy=true

if touched '^\.github/'; then
  play=true admin=true sites=true agent=true mc_server=true infra=true
fi

printf 'go=true\nplay=%s\nadmin=%s\nsites=%s\nagent=%s\nmc_server=%s\ninfra=%s\ndeploy=%s\n' \
  "$play" "$admin" "$sites" "$agent" "$mc_server" "$infra" "$deploy"
