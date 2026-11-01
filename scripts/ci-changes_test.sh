#!/usr/bin/env bash
# Tests for ci-changes.sh. Each case lists changed files and the scopes
# that must be on; every other scope must be off. With Go installed, it
# also checks that a change to any web/internal package a track's binary
# imports turns that track on.
#
# Usage: scripts/ci-changes_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
failures=0
tracks='play admin sites agent'
all="$tracks"

check() {
  local name=$1 files=$2 want=$3 got scope expect
  got=$(printf '%b' "$files" | "$dir/ci-changes.sh")
  for scope in $tracks mc_server infra deploy; do
    expect=false
    [[ " $want " == *" $scope "* ]] && expect=true
    if ! grep -qx "$scope=$expect" <<<"$got"; then
      echo "FAIL $name: want $scope=$expect, got: $(tr '\n' ' ' <<<"$got")"
      failures=$((failures + 1))
    fi
  done
  grep -qx 'go=true' <<<"$got" || { echo "FAIL $name: go must always run"; failures=$((failures + 1)); }
}

check "play site"         'sites/play/app.js\n'                    'play deploy'
check "play command"      'web/cmd/play/main.go\n'                 'play deploy'
check "play-only package" 'web/internal/world/world.go\n'          'play deploy'
check "site package"      'web/internal/site/site.go\n'            'play admin deploy'
check "deploy package"    'web/internal/deploy/deploy.go\n'        'admin agent deploy'
check "deploy state"      'web/internal/deploy/state/state.go\n'   'admin agent deploy'
check "go.mod"            'web/go.mod\n'                           "$all deploy"
check "admin site"        'sites/admin/index.html\n'               'admin deploy'
check "admin command"     'web/cmd/admin/main.go\n'                'admin deploy'
check "shared stylesheet" 'sites/shared/base.css\n'                'play admin deploy'
check "serve command"     'web/cmd/serve/main.go\n'                'sites deploy'
check "portfolio"         'sites/portfolio/index.html\n'           'sites deploy'
check "site settings"     'deploy/sites/portfolio.env\n'           'sites infra deploy'
check "agent only"        'web/cmd/mc-agent/main.go\n'             'agent deploy'
check "agent timer"       'deploy/systemd/mc-agent.timer\n'        'agent infra deploy'
check "lazymc config"     'lazymc.toml\n'                          'mc_server'
check "paper config"      'config/paper-global.yml\n'              'mc_server'
check "plugin config"     'plugins/Chunky/config.yml\n'            'mc_server'
check "start script"      'start.sh\n'                             'mc_server'
check "unit file"         'deploy/systemd/play.service\n'          'play infra deploy'
check "hand-run unit"     'deploy/systemd/minecraft.service\n'     'infra deploy'
check "track list"        'deploy/tracks.json\n'                   "$all infra deploy"
check "build script"      'scripts/build-release.sh\n'             "$all infra deploy"
check "other script"      'scripts/ci-changes.sh\n'                'infra'
check "workflow"          '.github/workflows/ci.yml\n'             "$all mc_server infra deploy"
check "readme only"       'README.md\n'                            ''
check "nothing"           ''                                       ''
check "lookalike paths"   'web/cmd/player/x.go\nsites/playground/a\nmy-start.sh\nweb/internal/sitex/a.go\n' 'deploy'
check "mixed"             'sites/play/style.css\nlazymc.toml\n'    'play mc_server deploy'
check "non-ASCII path"    'sites/play/caf\xc3\xa9.png\n'           'play deploy'

# git quotes paths it can't print plainly; the script must refuse them.
if printf '"sites/play/a\\"b.png"\n' | "$dir/ci-changes.sh" >/dev/null 2>&1; then
  echo "FAIL quoted path: want a non-zero exit"
  failures=$((failures + 1))
fi

# Every web/internal package a track's binary imports must turn it on.
if command -v go >/dev/null; then
  web="$dir/../web"
  mod=$(cd "$web" && go list -m)
  for track in $tracks; do
    for cmd in $(jq -r --arg t "$track" '.[$t].bins[]' "$dir/../deploy/tracks.json"); do
      for pkg in $(cd "$web" && go list -deps "./cmd/$cmd" | sed -n "s#^$mod/internal/##p"); do
        got=$(printf 'web/internal/%s/x.go\n' "$pkg" | "$dir/ci-changes.sh")
        if ! grep -qx "$track=true" <<<"$got"; then
          echo "FAIL $cmd imports internal/$pkg, but a change to it doesn't release $track"
          failures=$((failures + 1))
        fi
      done
    done
  done
else
  echo "skipping the import check: go is not installed"
fi

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "ci-changes: all cases pass"
