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
#
# If LazyMC exits by itself (a crash), Paper is stopped the same way, and
# this script exits with LazyMC's status, so Docker's restart policy starts
# the container again instead of leaving it up with nothing on the port.
#
# MC_HOME (default /opt/minecraft) and MC_SERVER (default /server) are for
# scripts/entrypoint_test.sh.
set -euo pipefail

home=${MC_HOME:-/opt/minecraft}
cd "${MC_SERVER:-/server}"

# setsid gives LazyMC, and Paper after it, a process group of their own,
# whose ID is LazyMC's PID.
setsid "$home/lazymc" start --config "$home/lazymc.toml" &
lazymc=$!

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

# LazyMC is gone. Whatever is left in its group is Paper: still saving
# after a stop, or orphaned by a crash. Stop it either way (a second
# SIGTERM to a Paper already saving is harmless), then wait for it. When
# this script exits, the container ends and the kernel kills everything
# left in it, so Paper must be done first, as systemd waited for the whole
# cgroup. Docker's stop_grace_period still bounds the wait after a stop.
stop
while kill -0 -- "-$lazymc" 2>/dev/null; do
  sleep 1
done
exit "$status"
