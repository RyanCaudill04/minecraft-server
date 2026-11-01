#!/usr/bin/env bash
# Tests for scripts/mc. Fake ssh, rsync and go on PATH record what they
# were asked to run, so nothing contacts the Pi.
#
# Usage: scripts/mc_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d "${TEST_TMPDIR:-${TMPDIR:-/tmp}}/mc_test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
failures=0

mkdir "$tmp/bin"
calls=$tmp/calls
cat >"$tmp/bin/ssh" <<'EOF'
#!/usr/bin/env bash
echo "ssh $*" >>"$CALLS"
for arg in "$@"; do
  [ "$arg" = -N ] && exec sleep 30 # the preview tunnel
done
if [[ " $* " == *" bash -s "* ]]; then
  cat >>"$CALLS" # the script sent on stdin
fi
EOF
cat >"$tmp/bin/rsync" <<'EOF'
#!/usr/bin/env bash
echo "rsync $*" >>"$CALLS"
EOF
cat >"$tmp/bin/go" <<'EOF'
#!/usr/bin/env bash
echo "go $* (in $(basename "$PWD"))" >>"$CALLS"
EOF
chmod +x "$tmp/bin/"*
export PATH="$tmp/bin:$PATH" CALLS=$calls MC_PREVIEW_DIR=$tmp/preview
unset MC_HOST

# run calls mc with args, stdin from /dev/null (so no terminal to ask on),
# and keeps its exit status in $status and its output in $out.
run() {
  : >"$calls"
  status=0
  out=$("$dir/mc" "$@" </dev/null 2>&1) || status=$?
}

# check fails the test name unless the exit status is want and the calls
# log matches each of the remaining extended regexps.
check() {
  local name=$1 want=$2 pattern
  shift 2
  if [ "$status" != "$want" ]; then
    echo "FAIL $name: exit $status, want $want; output: $out"
    failures=$((failures + 1))
    return
  fi
  for pattern in "$@"; do
    if ! grep -qE -- "$pattern" "$calls"; then
      echo "FAIL $name: no call matching $pattern; calls:"
      sed 's/^/    /' "$calls"
      failures=$((failures + 1))
    fi
  done
}

# no_calls fails name if anything was run.
no_calls() {
  if [ -s "$calls" ]; then
    echo "FAIL $1: expected no calls, got:"
    sed 's/^/    /' "$calls"
    failures=$((failures + 1))
  fi
}

run --help
check "help" 0
grep -q '^usage: scripts/mc' <<<"$out" || { echo "FAIL help: no usage line"; failures=$((failures + 1)); }
no_calls "help"

run
check "no command" 2
no_calls "no command"

run frobnicate
check "unknown command" 2
no_calls "unknown command"

run status
check "status" 0 '^ssh mc-pi .*list-units' 'mc-agent status' 'api/status'

MC_HOST=other-pi run version
check "MC_HOST" 0 '^ssh other-pi .*mc-agent version'

run status extra
check "status with args" 2
no_calls "status with args"

run logs
check "logs default" 0 '^ssh mc-pi journalctl --user -u minecraft -n 100 --no-pager$'

run logs play.service -n 5
check "logs unit" 0 '^ssh mc-pi journalctl --user -u play.service -n 5 --no-pager$'

run logs -f admin
check "logs follow" 0 '^ssh -t mc-pi journalctl --user -u admin -n 100 --no-pager -f$'

run logs 'x;reboot'
check "logs bad unit" 2
no_calls "logs bad unit"

run logs -n lots
check "logs bad count" 2
no_calls "logs bad count"

for cmd in start stop restart deploy rollback pause resume; do
  run "$cmd"
  check "$cmd without --yes" 2
  no_calls "$cmd without --yes"
done

run --yes restart
check "restart" 0 '^ssh mc-pi systemctl --user restart minecraft.service'
run -y stop
check "stop" 0 '^ssh mc-pi systemctl --user stop minecraft.service'
run -y start
check "start" 0 '^ssh mc-pi systemctl --user start minecraft.service'
run --yes deploy
check "deploy" 0 'systemctl --user start mc-agent.service' 'journalctl --user -u mc-agent.service'
run --yes rollback
check "rollback" 0 'mc-agent rollback'
grep -q "scripts/mc pause" <<<"$out" || { echo "FAIL rollback: no hint about pause"; failures=$((failures + 1)); }
run --yes pause
check "pause" 0 '^ssh mc-pi systemctl --user stop mc-agent.timer'
run --yes resume
check "resume" 0 '^ssh mc-pi systemctl --user start mc-agent.timer'

run cleanup-legacy
check "cleanup dry run" 0 '^ssh mc-pi bash -s --$' 'pi-cleanup-legacy'
run cleanup-legacy --apply
check "cleanup apply without --yes" 2
no_calls "cleanup apply without --yes"
run --yes cleanup-legacy --apply --backups
check "cleanup apply" 0 '^ssh mc-pi bash -s -- --apply --backups$'

run preview
check "preview" 0 \
  "^rsync -a --delete .* mc-pi:minecraft_server/ $tmp/preview/\$" \
  '^ssh -N .*-L 127.0.0.1:35565:127.0.0.1:25565 .* mc-pi$' \
  "^go run ./cmd/play .*-server $tmp/preview .*-lazymc 127.0.0.1:35565 -paper 127.0.0.1:35566 \(in web\)\$"

run preview --no-sync
check "preview without sync" 0 '^go run ./cmd/play'
if grep -q '^rsync' "$calls"; then
  echo "FAIL preview --no-sync: rsync ran"
  failures=$((failures + 1))
fi

if [ "$failures" -gt 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "mc: all tests passed"
