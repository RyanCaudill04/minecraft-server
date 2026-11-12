#!/bin/bash
# The mc-server container's command: runs LazyMC, which owns the public
# port and starts Paper (start.sh) on demand, and passes a stop on to both.
#
# docker stop signals only this script (compose.yaml's init runs it). LazyMC
# neither forwards signals to Paper nor quits on SIGINT, and LazyMC sleeps
# Paper by freezing it with SIGSTOP. So a stop sends SIGTERM to LazyMC's
# whole process group, then SIGCONT so a frozen Paper wakes to handle it:
# what systemd's KillMode=control-group did for minecraft.service. Paper
# saves and exits, then LazyMC exits. compose.yaml's stop_grace_period
# allows longer than LazyMC's stop_timeout before Docker kills what's left.
set -euo pipefail

cd /server

# setsid gives LazyMC, and Paper after it, a process group of their own,
# whose ID is LazyMC's PID.
setsid /opt/minecraft/lazymc start --config /opt/minecraft/lazymc.toml &
lazymc=$!

# shellcheck disable=SC2329 # run by the trap below
stop() {
  kill -TERM -- "-$lazymc" 2>/dev/null || true
  kill -CONT -- "-$lazymc" 2>/dev/null || true
}
trap stop TERM INT

# wait returns early when a trapped signal arrives, so keep waiting until
# LazyMC has really exited.
status=0
while kill -0 "$lazymc" 2>/dev/null; do
  if wait "$lazymc"; then status=0; else status=$?; fi
done

# LazyMC exits at once on SIGTERM, while Paper may still be saving. When
# this script exits, the container ends and the kernel kills everything
# left in it, so wait for the rest of LazyMC's group (Paper) to exit too,
# as systemd waited for the whole cgroup. Docker's stop_grace_period still
# bounds the wait.
while kill -0 -- "-$lazymc" 2>/dev/null; do
  sleep 1
done
exit "$status"
