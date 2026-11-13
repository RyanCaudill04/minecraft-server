#!/usr/bin/env bash
# Tests for deploy/containers/mc-server/entrypoint.sh, with a fake LazyMC
# that starts a fake Paper in its process group, as the real one does.
# The fake Paper takes a moment to "save" on SIGTERM and records it.
#
# Needs setsid (Linux, as in the container); elsewhere it says so and
# skips.
#
# Usage: scripts/entrypoint_test.sh
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
entrypoint=$root/deploy/containers/mc-server/entrypoint.sh
if ! command -v setsid >/dev/null; then
  echo "entrypoint: skipped, no setsid here (CI runs it on Linux)"
  exit 0
fi

tmp=$(mktemp -d)
trap 'pkill -KILL -f "$tmp/" 2>/dev/null || true; rm -rf "$tmp"' EXIT
failures=0
fail() { echo "FAIL $*"; failures=$((failures + 1)); }

cat >"$tmp/paper" <<'EOF'
#!/bin/bash
echo $$ >"$FAKE_DIR/paper.pid"
trap 'sleep 1; echo saved >>"$FAKE_DIR/saved"; exit 0' TERM
while :; do sleep 0.1; done
EOF
cat >"$tmp/lazymc" <<'EOF'
#!/bin/bash
# Like LazyMC: Paper runs in the same process group, and SIGTERM ends
# LazyMC at once without waiting for Paper.
"$FAKE_DIR/paper" &
case $FAKE_MODE in
  crash) sleep 1; exit 3 ;;
  *) trap 'exit 0' TERM; while :; do sleep 0.1; done ;;
esac
EOF
chmod +x "$tmp/paper" "$tmp/lazymc"
touch "$tmp/lazymc.toml"

# run MODE [freeze]: starts the entrypoint, stops it unless MODE is crash,
# and sets $status and $took (seconds until it exited).
run() {
  local mode=$1 freeze=${2:-} pid start
  rm -f "$tmp/saved" "$tmp/paper.pid"
  start=$(date +%s)
  FAKE_DIR=$tmp FAKE_MODE=$mode MC_HOME=$tmp MC_SERVER=$tmp "$entrypoint" &
  pid=$!
  for _ in $(seq 50); do [ -s "$tmp/paper.pid" ] && break; sleep 0.1; done
  [ -s "$tmp/paper.pid" ] || { fail "$mode: fake Paper never started"; return; }
  if [ -n "$freeze" ]; then
    kill -STOP "$(cat "$tmp/paper.pid")"
  fi
  [ "$mode" = crash ] || kill -TERM "$pid"
  for _ in $(seq 150); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
  if kill -0 "$pid" 2>/dev/null; then
    fail "$mode${freeze:+ (frozen)}: still running after 15 s"
    kill -KILL "$pid"
  fi
  status=0
  wait "$pid" || status=$?
  took=$(($(date +%s) - start))
}

paper_gone() { ! kill -0 "$(cat "$tmp/paper.pid")" 2>/dev/null; }

# A stop: Paper saves before the entrypoint exits, and the exit is clean.
run stop
[ -f "$tmp/saved" ] || fail "stop: Paper didn't save before the entrypoint exited"
paper_gone || fail "stop: Paper is still running"
[ "$status" = 0 ] || fail "stop: exit status $status, want 0"

# A stop while LazyMC has Paper frozen: SIGCONT wakes it to save.
run stop freeze
[ -f "$tmp/saved" ] || fail "frozen: Paper didn't save"
paper_gone || fail "frozen: Paper is still running"

# LazyMC crashes with Paper running: the entrypoint doesn't wait forever on
# the orphaned Paper. It stops it (Paper saves) and exits with LazyMC's
# status, so Docker restarts the container.
run crash
[ -f "$tmp/saved" ] || fail "crash: the orphaned Paper wasn't stopped cleanly"
paper_gone || fail "crash: Paper is still running"
[ "$status" = 3 ] || fail "crash: exit status $status, want LazyMC's 3"
((took < 10)) || fail "crash: took ${took}s to exit"

# The same with Paper frozen when LazyMC dies.
run crash freeze
[ -f "$tmp/saved" ] || fail "crash, frozen: Paper wasn't woken to save"
[ "$status" = 3 ] || fail "crash, frozen: exit status $status, want 3"

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "entrypoint: all cases pass"
