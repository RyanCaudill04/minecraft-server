#!/usr/bin/env bash
# Tests for deploy/backup/mc-backup.sh against a throwaway fake server
# folder: a backup restores to the same files, the 10-day cleanup and keep
# flags over weeks of fake dates, and the Paper-running cases with and
# without RCON.
#
# Usage: scripts/mc-backup_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
backup=$dir/../deploy/backup/mc-backup.sh
tmp=$(mktemp -d)
fake_paper=
cleanup() {
  [[ -n $fake_paper ]] && kill "$fake_paper" 2>/dev/null
  rm -rf "$tmp"
}
trap cleanup EXIT
failures=0
fail() {
  echo "FAIL $*"
  failures=$((failures + 1))
}

# A small server folder: a world in Paper's split layout, the runtime files
# that aren't in git, and something that must not be backed up.
server=$tmp/server
mkdir -p "$server/world/region" "$server/world_nether/DIM-1/region" "$server/logs"
head -c 4096 /dev/urandom >"$server/world/level.dat"
head -c 65536 /dev/urandom >"$server/world/region/r.0.0.mca"
head -c 8192 /dev/urandom >"$server/world_nether/DIM-1/region/r.0.0.mca"
printf 'level-name=world\nenable-rcon=false\nrcon.port=25575\nrcon.password=\n' >"$server/server.properties"
echo '[]' >"$server/ops.json"
echo 'log' >"$server/logs/latest.log"

# Nothing in the test environment looks like Paper unless a case starts it.
export MC_SERVER_DIR=$server MC_PAPER_PATTERN="fake-paper-$$"

run() { # run <backup dir> <timestamp> [VAR=value...]
  local out=$1 stamp=$2
  shift 2
  env MC_BACKUP_DIR="$out" MC_BACKUP_TIME="$stamp" "$@" "$backup" >>"$tmp/log" 2>&1
}

start_paper() {
  bash -c "exec -a fake-paper-$$ sleep 60" &
  fake_paper=$!
  sleep 0.2
}
stop_paper() {
  kill "$fake_paper" 2>/dev/null || true
  wait "$fake_paper" 2>/dev/null || true
  fake_paper=
}

# 1. A backup with Paper asleep, checked and restored.
out=$tmp/one
if run "$out" 20261031-043000; then
  a=$out/world-20261031-043000.tar.gz
  [[ -f $a && -f $a.sha256 ]] || fail "asleep: archive or checksum missing"
  (cd "$out" && { sha256sum -c "${a##*/}.sha256" 2>/dev/null || shasum -a 256 -c "${a##*/}.sha256"; }) >/dev/null ||
    fail "asleep: checksum doesn't match"
  mkdir -p "$tmp/restore"
  tar -xzf "$a" -C "$tmp/restore"
  for f in world world_nether server.properties ops.json; do
    diff -r "$server/$f" "$tmp/restore/$f" >/dev/null || fail "asleep: restored $f differs"
  done
  [[ ! -e $tmp/restore/logs ]] || fail "asleep: logs/ shouldn't be backed up"
  [[ $(stat -c %a "$a" 2>/dev/null || stat -f %Lp "$a") == 600 ]] || fail "asleep: archive should be mode 600"
  [[ -z $(find "$out" -name '*.partial' -o -name .staging) ]] || fail "asleep: left temporary files"
  # The same timestamp again must not overwrite it.
  if run "$out" 20261031-043000; then fail "asleep: overwrote an existing archive"; fi
else
  fail "asleep: backup failed"
fi

# 2. Retention: one backup a day at 04:30 from Oct 1 to Oct 20 2026, with
# Oct 3 kept for safe keeping, and a stray flag for an archive that's gone.
# Each run removes backups 10 days old or older.
out=$tmp/retention
mkdir -p "$out"
for day in $(seq 0 19); do
  stamp=$(date -u -j -v+"${day}"d -f %Y%m%d 20261001 +%Y%m%d 2>/dev/null || date -u -d "20261001 +$day days" +%Y%m%d)
  run "$out" "$stamp-043000" MC_BACKUP_KEEP_DAYS=10 || fail "retention: backup $stamp failed"
  [[ $stamp == 20261003 ]] && touch "$out/world-20261003-043000.tar.gz.keep"
done
touch "$out/world-20260101-043000.tar.gz.keep"
# Just under 10 days after Oct 11 04:30, then exactly 10 days.
run "$out" 20261021-042959 MC_BACKUP_KEEP_DAYS=10 || fail "retention: Oct 21 04:29 backup failed"
[[ -f $out/world-20261011-043000.tar.gz ]] || fail "retention: removed a backup under 10 days old"
run "$out" 20261021-043000 MC_BACKUP_KEEP_DAYS=10 || fail "retention: Oct 21 04:30 backup failed"
want="world-20261003-043000.tar.gz
world-20261012-043000.tar.gz
world-20261013-043000.tar.gz
world-20261014-043000.tar.gz
world-20261015-043000.tar.gz
world-20261016-043000.tar.gz
world-20261017-043000.tar.gz
world-20261018-043000.tar.gz
world-20261019-043000.tar.gz
world-20261020-043000.tar.gz
world-20261021-042959.tar.gz
world-20261021-043000.tar.gz"
got=$(cd "$out" && printf '%s\n' *.tar.gz | sort)
[[ $got == "$want" ]] || fail "retention: kept $(echo "$got" | tr '\n' ' ')"
[[ $(cd "$out" && printf '%s\n' *.sha256 | wc -l | tr -d ' ') == 12 ]] || fail "retention: checksum files weren't removed with their archives"
[[ -f $out/world-20261003-043000.tar.gz.keep ]] || fail "retention: removed the keep flag of a kept backup"
[[ ! -e $out/world-20260101-043000.tar.gz.keep ]] || fail "retention: left a flag for a missing archive"
# Files that aren't backups are never removed, however old.
touch "$out/notes.txt" "$out/world-old-copy.tar.gz"
run "$out" 20261201-043000 || fail "retention: Dec 1 backup failed"
[[ -f $out/notes.txt && -f $out/world-old-copy.tar.gz ]] || fail "retention: removed a file that isn't a backup"
# The default is 10 days too, and only the kept and the new backup are left.
got=$(cd "$out" && printf '%s\n' world-2*.tar.gz | sort | tr '\n' ' ')
[[ $got == "world-20261003-043000.tar.gz world-20261201-043000.tar.gz " ]] || fail "retention: default kept $got"
# The old daily and weekly settings only warn.
run "$out" 20261202-043000 BACKUP_KEEP_DAILY=7 || fail "retention: an old setting failed the backup"
grep -q "BACKUP_KEEP_DAILY and BACKUP_KEEP_WEEKLY are no longer used" "$tmp/log" || fail "retention: no warning for an old setting"

# 3. Paper running without RCON: wait, then give up without an archive.
out=$tmp/no-rcon
start_paper
if run "$out" 20261031-050000 MC_BACKUP_WAIT=0; then fail "no rcon: should fail while Paper runs"; fi
[[ ! -e $out/world-20261031-050000.tar.gz ]] || fail "no rcon: wrote an archive"

# 4. Paper running with RCON: saving is paused around the copy.
fake_rcon=$tmp/fake-rcon
cat >"$fake_rcon" <<'EOF'
#!/usr/bin/env bash
[[ $RCON_PASSWORD == hunter2 ]] || exit 3
echo "$*" >>"$RCON_LOG"
[[ " $* " != *" save-off "* || -z ${RCON_FAIL:-} ]] || exit 1
[[ " $* " != *" save-on "* || -z ${SAVEON_FAIL:-} ]] || exit 1
# RESTART_FLAG: replace Paper with a new process, once, as LazyMC would.
if [[ " $* " == *" save-off "* && -n ${RESTART_FLAG:-} && -e $RESTART_FLAG ]]; then
  rm -f "$RESTART_FLAG"
  kill "$(cat "$PID_FILE")"
  bash -c "exec -a $MC_PAPER_PATTERN sleep 60" >/dev/null 2>&1 &
  echo $! >"$PID_FILE"
  sleep 0.2
fi
EOF
chmod +x "$fake_rcon"
printf 'level-name=world\nenable-rcon=true\nrcon.port=25575\nrcon.password=hunter2\n' >"$server/server.properties"
out=$tmp/rcon
run "$out" 20261031-060000 MC_RCON="$fake_rcon" RCON_LOG="$tmp/rcon.log" || fail "rcon: backup failed"
[[ -f $out/world-20261031-060000.tar.gz ]] || fail "rcon: no archive"
want="-addr 127.0.0.1:25575 -timeout 120s save-off
-addr 127.0.0.1:25575 -timeout 120s save-all flush
-addr 127.0.0.1:25575 -timeout 120s save-on"
[[ $(cat "$tmp/rcon.log") == "$want" ]] || fail "rcon: commands were $(tr '\n' '|' <"$tmp/rcon.log")"

# 5. RCON failing while Paper runs: no archive, and save-on is still sent.
rm -f "$tmp/rcon.log"
if run "$out" 20261031-070000 MC_RCON="$fake_rcon" RCON_LOG="$tmp/rcon.log" RCON_FAIL=1; then
  fail "rcon failure: should fail"
fi
[[ ! -e $out/world-20261031-070000.tar.gz ]] || fail "rcon failure: wrote an archive"
[[ $(tail -1 "$tmp/rcon.log") == "-addr 127.0.0.1:25575 -timeout 120s save-on" ]] || fail "rcon failure: save-on wasn't sent"

# 5b. Paper replaced during the copy (LazyMC slept and woke it): the copy is
# thrown away and redone, so the archive comes from one steady Paper.
rm -f "$tmp/rcon.log"
echo "$fake_paper" >"$tmp/paper.pid"
touch "$tmp/restart.flag"
run "$out" 20261031-071000 MC_RCON="$fake_rcon" RCON_LOG="$tmp/rcon.log" \
  RESTART_FLAG="$tmp/restart.flag" PID_FILE="$tmp/paper.pid" || fail "restart: backup failed"
[[ -f $out/world-20261031-071000.tar.gz ]] || fail "restart: no archive"
[[ $(grep -c save-off "$tmp/rcon.log") == 2 ]] || fail "restart: expected a retry, commands were $(tr '\n' '|' <"$tmp/rcon.log")"
fake_paper=$(cat "$tmp/paper.pid")

# 5c. save-on failing is an error, not a good backup.
if run "$out" 20261031-072000 MC_RCON="$fake_rcon" RCON_LOG="$tmp/rcon.log" SAVEON_FAIL=1; then
  fail "save-on failure: should fail"
fi
[[ ! -e $out/world-20261031-072000.tar.gz ]] || fail "save-on failure: wrote an archive"
stop_paper

# 5d. Not enough free space fails before anything is copied; a leftover
# .partial from a killed run is removed.
out=$tmp/space
mkdir -p "$out"
touch "$out/world-20250101-000000.tar.gz.partial"
if run "$out" 20261031-073000 MC_BACKUP_MIN_FREE_KB=999999999999; then fail "space: should fail"; fi
grep -q "not enough free space" "$tmp/log" || fail "space: no clear message"
[[ ! -e $out/world-20261031-073000.tar.gz && ! -e $out/.staging ]] || fail "space: copied anyway"
[[ ! -e $out/world-20250101-000000.tar.gz.partial ]] || fail "partial: leftover wasn't removed"

# 5e. The shared host.env names work when the MC_ ones aren't set.
out=$tmp/hostenv
if (unset MC_SERVER_DIR; run "$out" 20261031-074000 SERVER_DIR="$server" BACKUP_KEEP_DAYS=1); then
  [[ -f $out/world-20261031-074000.tar.gz ]] || fail "host.env names: no archive"
else
  fail "host.env names: backup failed"
fi

# 6. A missing world is an error, not an empty backup.
mv "$server/world" "$server/world.moved"
if run "$tmp/missing" 20261031-080000; then fail "missing world: should fail"; fi
mv "$server/world.moved" "$server/world"

if ((failures)); then
  echo "--- backup log"
  cat "$tmp/log"
  echo "$failures failure(s)"
  exit 1
fi
echo "mc-backup: all cases pass"
