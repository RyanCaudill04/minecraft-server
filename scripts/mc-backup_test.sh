#!/usr/bin/env bash
# Tests for deploy/backup/mc-backup.sh against a throwaway fake server
# folder: a backup restores to the same files, retention over several weeks
# of fake dates, and the Paper-running cases with and without RCON.
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
  [[ $(stat -f %Lp "$a" 2>/dev/null || stat -c %a "$a") == 600 ]] || fail "asleep: archive should be mode 600"
  [[ -z $(find "$out" -name '*.partial' -o -name .staging) ]] || fail "asleep: left temporary files"
  # The same timestamp again must not overwrite it.
  if run "$out" 20261031-043000; then fail "asleep: overwrote an existing archive"; fi
else
  fail "asleep: backup failed"
fi

# 2. Retention: one backup a day from Sep 1 to Oct 10 2026 (a Saturday),
# plus a second one on Oct 10, keeping 7 daily and 4 weekly.
out=$tmp/retention
for day in $(seq 0 39); do
  stamp=$(date -u -j -v+"${day}"d -f %Y%m%d 20260901 +%Y%m%d 2>/dev/null || date -u -d "20260901 +$day days" +%Y%m%d)
  run "$out" "$stamp-043000" MC_BACKUP_KEEP_DAILY=7 MC_BACKUP_KEEP_WEEKLY=4 || fail "retention: backup $stamp failed"
done
run "$out" 20261010-120000 MC_BACKUP_KEEP_DAILY=7 MC_BACKUP_KEEP_WEEKLY=4 || fail "retention: second Oct 10 backup failed"
# Newest of each of the last 7 days (Oct 4-10), then the newest of each
# older week: Oct 4 already covers its week, then Sep 27 and Sep 20.
want="world-20260920-043000.tar.gz
world-20260927-043000.tar.gz
world-20261004-043000.tar.gz
world-20261005-043000.tar.gz
world-20261006-043000.tar.gz
world-20261007-043000.tar.gz
world-20261008-043000.tar.gz
world-20261009-043000.tar.gz
world-20261010-120000.tar.gz"
got=$(cd "$out" && printf '%s\n' *.tar.gz | sort)
[[ $got == "$want" ]] || fail "retention: kept $(echo "$got" | tr '\n' ' ')"
[[ $(cd "$out" && printf '%s\n' *.sha256 | wc -l | tr -d ' ') == 9 ]] || fail "retention: checksum files weren't pruned with their archives"
# Files that aren't backups are never pruned.
touch "$out/notes.txt" "$out/world-old-copy.tar.gz"
run "$out" 20261011-043000 || fail "retention: Oct 11 backup failed"
[[ -f $out/notes.txt && -f $out/world-old-copy.tar.gz ]] || fail "retention: pruned a file that isn't a backup"

# 3. Paper running without RCON: wait, then give up without an archive.
out=$tmp/no-rcon
start_paper
if run "$out" 20261031-050000 MC_BACKUP_WAIT=0; then fail "no rcon: should fail while Paper runs"; fi
[[ ! -e $out/world-20261031-050000.tar.gz ]] || fail "no rcon: wrote an archive"

# 4. Paper running with RCON: saving is paused around the copy.
fake_rcon=$tmp/fake-rcon
cat >"$fake_rcon" <<'EOF'
#!/usr/bin/env bash
[[ $MC_RCON_PASSWORD == hunter2 ]] || exit 3
echo "$*" >>"$RCON_LOG"
[[ " $* " != *" save-off "* || -z ${RCON_FAIL:-} ]] || exit 1
EOF
chmod +x "$fake_rcon"
printf 'level-name=world\nenable-rcon=true\nrcon.port=25575\nrcon.password=hunter2\n' >"$server/server.properties"
out=$tmp/rcon
run "$out" 20261031-060000 MC_RCON="$fake_rcon" RCON_LOG="$tmp/rcon.log" || fail "rcon: backup failed"
[[ -f $out/world-20261031-060000.tar.gz ]] || fail "rcon: no archive"
want="-addr 127.0.0.1:25575 save-off save-all flush
-addr 127.0.0.1:25575 save-on"
[[ $(cat "$tmp/rcon.log") == "$want" ]] || fail "rcon: commands were $(tr '\n' '|' <"$tmp/rcon.log")"

# 5. RCON failing while Paper runs: no archive, and save-on is still sent.
rm -f "$tmp/rcon.log"
if run "$out" 20261031-070000 MC_RCON="$fake_rcon" RCON_LOG="$tmp/rcon.log" RCON_FAIL=1; then
  fail "rcon failure: should fail"
fi
[[ ! -e $out/world-20261031-070000.tar.gz ]] || fail "rcon failure: wrote an archive"
[[ $(tail -1 "$tmp/rcon.log") == "-addr 127.0.0.1:25575 save-on" ]] || fail "rcon failure: save-on wasn't sent"
stop_paper

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
