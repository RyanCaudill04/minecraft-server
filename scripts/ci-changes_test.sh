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
  for scope in play admin mc_server infra images deploy; do
    expect=false
    [[ " $want " == *" $scope "* ]] && expect=true
    if ! grep -qx "$scope=$expect" <<<"$got"; then
      echo "FAIL $name: want $scope=$expect, got: $(tr '\n' ' ' <<<"$got")"
      failures=$((failures + 1))
    fi
  done
}

check "play site"         'sites/play/app.js\n'                    'play images deploy'
check "play tests"        'tests/play/e2e/play.spec.js\n'         'play'
check "play command"      'web/cmd/play/main.go\n'                 'play images deploy'
check "shared Go code"    'web/internal/world/world.go\n'          'play admin images deploy'
check "go.mod"            'web/go.mod\n'                           'play admin images deploy'
check "admin site"        'sites/admin/index.html\n'               'admin images deploy'
check "admin command"     'web/cmd/admin/main.go\n'                'admin images deploy'
check "admin tests"       'tests/admin/status.test.js\n'          'admin'
check "shared stylesheet" 'sites/shared/base.css\n'                'play admin images deploy'
check "agent only"        'web/cmd/mc-agent/main.go\n'             'images deploy'
check "lazymc config"     'deploy/containers/mc-server/lazymc.toml\n' 'mc_server infra images deploy'
check "paper config"      'config/paper-global.yml\n'              'mc_server'
check "plugin config"     'plugins/Chunky/config.yml\n'            'mc_server'
check "start script"      'deploy/containers/mc-server/start.sh\n' 'mc_server infra images deploy'
check "old root files"    'lazymc.toml\nstart.sh\n'               ''
check "server manifest"   'deploy/server.json\n'                   'mc_server infra images deploy'
check "mc-players"        'web/cmd/mc-players/main.go\n'           'mc_server'
check "dev Worker"        'dev/worker.mjs\n'                       'infra'
check "dev Worker tests"  'tests/dev/worker.test.mjs\n'            'infra'
check "dev build script"  'scripts/build-dev.sh\n'                 'infra'
check "mc-players + play" 'web/cmd/mc-players/main.go\nweb/cmd/play/main.go\n' 'play mc_server images deploy'
check "fetch script"      'scripts/fetch-server.sh\n'              'mc_server infra images deploy'
check "images script"     'scripts/images.sh\n'                    'infra images deploy'
check "web Dockerfile"    'deploy/containers/web/Dockerfile\n'     'infra images deploy'
check "compose file"      'deploy/containers/compose.yaml\n'       'infra images deploy'
check "dockerignore"      '.dockerignore\n'                        'images deploy'
check "unit file"         'deploy/systemd/mc-containers.service\n' 'infra deploy'
check "build script"      'scripts/build-release.sh\n'             'infra deploy'
check "other script"      'scripts/ci-changes.sh\n'                'infra'
check "workflow"          '.github/workflows/ci.yml\n'             'play admin mc_server infra images deploy'
check "readme only"       'README.md\n'                            ''
check "nothing"           ''                                       ''
check "lookalike paths"   'web/cmd/player/x.go\nsites/playground/a\nmy-start.sh\n' 'images deploy'
check "mixed"             'sites/play/style.css\nbukkit.yml\n'     'play mc_server images deploy'
check "non-ASCII path"    'sites/play/caf\xc3\xa9.png\n'           'play images deploy'

# git quotes paths it can't print plainly; the script must refuse them.
if printf '"sites/play/a\\"b.png"\n' | "$dir/ci-changes.sh" >/dev/null 2>&1; then
  echo "FAIL quoted path: want a non-zero exit"
  failures=$((failures + 1))
fi

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "ci-changes: all cases pass"
