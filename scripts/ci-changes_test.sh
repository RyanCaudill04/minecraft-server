#!/usr/bin/env bash
# Tests for ci-changes.sh. Each case lists changed files and the scopes
# that must be on; every other scope must be off.
#
# Usage: scripts/ci-changes_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
failures=0

check() {
  local name=$1 files=$2 want=$3 got scope expect
  got=$(printf '%b' "$files" | "$dir/ci-changes.sh")
  for scope in play admin mc_server infra deploy; do
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
check "shared Go code"    'web/internal/world/world.go\n'          'play admin deploy'
check "go.mod"            'web/go.mod\n'                           'play admin deploy'
check "admin site"        'sites/admin/index.html\n'               'admin deploy'
check "serve command"     'web/cmd/serve/main.go\n'                'admin deploy'
check "portfolio"         'sites/portfolio/index.html\n'           'admin deploy'
check "agent only"        'web/cmd/mc-agent/main.go\n'             'deploy'
check "lazymc config"     'lazymc.toml\n'                          'mc_server'
check "paper config"      'config/paper-global.yml\n'              'mc_server'
check "plugin config"     'plugins/Chunky/config.yml\n'            'mc_server'
check "start script"      'start.sh\n'                             'mc_server'
check "unit file"         'deploy/systemd/play.service\n'          'infra deploy'
check "build script"      'scripts/build-release.sh\n'             'infra deploy'
check "other script"      'scripts/ci-changes.sh\n'                'infra'
check "workflow"          '.github/workflows/ci.yml\n'             'play admin mc_server infra deploy'
check "readme only"       'README.md\n'                            ''
check "nothing"           ''                                       ''
check "lookalike paths"   'web/cmd/player/x.go\nsites/playground/a\nmy-start.sh\n' 'deploy'
check "mixed"             'sites/play/style.css\nlazymc.toml\n'    'play mc_server deploy'

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "ci-changes: all cases pass"
