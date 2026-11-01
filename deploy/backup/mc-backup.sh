#!/usr/bin/env bash
# Backs up the Minecraft world to a timestamped .tar.gz, checks the archive,
# and prunes old ones. Run nightly by mc-backup.timer; safe to run by hand.
#
# Getting a consistent copy:
#   - Paper asleep (LazyMC stopped it): nothing writes the world, so it is
#     copied as-is. If Paper starts during the copy, the copy is retried.
#   - Paper awake, RCON enabled in server.properties: "save-off" and
#     "save-all flush" over RCON, copy, then "save-on" (always, even on
#     failure).
#   - Paper awake, no RCON: wait up to MC_BACKUP_WAIT seconds for it to
#     sleep, then give up. A copy of region files Paper is writing can hold
#     torn chunks, and pruning would count it as a good backup.
#
# The world is copied to a staging folder first, so saving is paused only
# for the copy, not for the compression.
#
# Settings (environment, or ~/.config/mc-backup.env for the systemd unit):
#   MC_SERVER_DIR          server folder (default ~/minecraft_server)
#   MC_BACKUP_DIR          where archives go (default ~/backups/minecraft)
#   MC_BACKUP_KEEP_DAILY   newest backup of each of the last N days (default 7)
#   MC_BACKUP_KEEP_WEEKLY  newest backup of each of the last M weeks (default 4)
#   MC_BACKUP_WAIT         seconds to wait for Paper to sleep without RCON (default 1800)
#   MC_BACKUP_COPY_TO      optional rsync destination for an off-Pi copy, e.g.
#                          user@nas:backups/minecraft/ (new archives only; it is
#                          never pruned by this script)
#   MC_RCON                mc-rcon binary (default: bin/mc-rcon of this release)
#   MC_PAPER_PATTERN       pgrep -f pattern that means Paper is running
#   MC_BACKUP_TIME         timestamp to use, YYYYmmdd-HHMMSS (for tests)
#
# Archives are named <level-name>-YYYYmmdd-HHMMSS.tar.gz, with a .sha256 file
# next to each. Restore: see README.md.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
server_dir=${MC_SERVER_DIR:-$HOME/minecraft_server}
backup_dir=${MC_BACKUP_DIR:-$HOME/backups/minecraft}
keep_daily=${MC_BACKUP_KEEP_DAILY:-7}
keep_weekly=${MC_BACKUP_KEEP_WEEKLY:-4}
wait_secs=${MC_BACKUP_WAIT:-1800}
copy_to=${MC_BACKUP_COPY_TO:-}
rcon_bin=${MC_RCON:-$here/../../bin/mc-rcon}
paper_pattern=${MC_PAPER_PATTERN:-org.bukkit.craftbukkit.Main}
stamp=${MC_BACKUP_TIME:-$(date +%Y%m%d-%H%M%S)}

log() { echo "mc-backup: $*"; }
die() { echo "mc-backup: $*" >&2; exit 1; }

for n in "$keep_daily" "$keep_weekly" "$wait_secs"; do
  [[ $n =~ ^[0-9]+$ ]] || die "retention and wait settings must be whole numbers, got '$n'"
done
((keep_daily >= 1)) || die "MC_BACKUP_KEEP_DAILY must be at least 1, or the new backup would be pruned"
[[ $stamp =~ ^[0-9]{8}-[0-9]{6}$ ]] || die "timestamp '$stamp' is not YYYYmmdd-HHMMSS"

props=$server_dir/server.properties
[[ -f $props ]] || die "$props not found; set MC_SERVER_DIR"

# prop KEY prints KEY's value from server.properties (exact key match).
prop() { awk -v k="$1=" 'index($0, k) == 1 { v = substr($0, length(k) + 1) } END { print v }' "$props"; }

level=$(prop level-name)
level=${level:-world}
[[ $level =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "unsupported level-name '$level'"
[[ -f $server_dir/$level/level.dat ]] || die "$server_dir/$level/level.dat not found; nothing to back up"

# Every world folder Paper keeps, plus the small runtime files git doesn't
# track (server.properties holds the RCON password, hence umask 077).
umask 077
sources=()
for f in "$level" "${level}_nether" "${level}_the_end" \
  server.properties ops.json whitelist.json banned-players.json banned-ips.json usercache.json; do
  [[ -e $server_dir/$f ]] && sources+=("$f")
done

rcon_password=
if [[ $(prop enable-rcon) == true ]]; then
  rcon_password=$(prop rcon.password)
  rcon_addr=127.0.0.1:$(prop rcon.port)
  [[ -n $rcon_password ]] || log "enable-rcon=true but rcon.password is empty; treating RCON as off"
fi

mkdir -p "$backup_dir"
if command -v flock >/dev/null; then
  exec 9>"$backup_dir/.lock"
  flock -n 9 || die "another backup is running"
fi

name=$level-$stamp.tar.gz
archive=$backup_dir/$name
[[ ! -e $archive ]] || die "$archive already exists"
staging=$backup_dir/.staging
partial=$archive.partial
saving_off=false

rcon() { MC_RCON_PASSWORD=$rcon_password "$rcon_bin" -addr "$rcon_addr" "$@"; }

resume_saving() {
  if $saving_off; then
    saving_off=false
    rcon save-on >/dev/null || log "warning: save-on failed (fine if Paper has since stopped)"
  fi
}

cleanup() {
  resume_saving
  rm -rf "$staging" "$partial"
}
trap cleanup EXIT

paper_running() { pgrep -f "$paper_pattern" >/dev/null; }

# copy_world copies the sources into the staging folder, taking care that
# Paper isn't writing them. It returns 1 if the copy has to be retried.
# It runs as an "until" condition, where set -e is off, so every step that
# can fail checks for it.
copy_world() {
  local mode=asleep
  { rm -rf "$staging" && mkdir -p "$staging"; } || die "can't create $staging"

  if paper_running; then
    if [[ -n $rcon_password ]]; then
      mode=rcon
      log "Paper is running; pausing saves over RCON"
      # Set first: if save-off works but the flush fails, the exit trap
      # still turns saving back on.
      saving_off=true
      if ! rcon save-off "save-all flush" >/dev/null; then
        paper_running && die "RCON failed while Paper is running; check enable-rcon, rcon.port and rcon.password"
        saving_off=false
        log "Paper stopped before RCON answered; retrying"
        return 1
      fi
    else
      log "Paper is running and RCON is off; waiting up to ${wait_secs}s for it to sleep"
      local waited=0
      while paper_running; do
        ((waited < wait_secs)) || die "Paper is still running; enable RCON (see README.md) or try again later"
        sleep 10
        waited=$((waited + 10))
      done
      log "Paper is asleep"
    fi
  fi

  (cd "$server_dir" && cp -a "${sources[@]}" "$staging/") || die "copying the world failed"

  if [[ $mode == rcon ]]; then
    resume_saving
  elif paper_running; then
    log "Paper started during the copy; retrying"
    return 1
  fi
  log "copied ${sources[*]} ($mode)"
}

attempt=1
until copy_world; do
  ((attempt < 3)) || die "couldn't get a consistent copy after $attempt attempts"
  attempt=$((attempt + 1))
done

tar -C "$staging" -czf "$partial" "${sources[@]}"
rm -rf "$staging"

# Verify before it counts as a backup: the gzip stream is intact, tar can
# read every entry, and the world's level.dat is in it.
gzip -t "$partial" || die "gzip check failed for $partial"
# grep without -q reads the whole listing, so tar never gets SIGPIPE.
tar -tzf "$partial" | grep -x "$level/level.dat" >/dev/null || die "$partial has no $level/level.dat"

mv "$partial" "$archive"
if command -v sha256sum >/dev/null; then
  (cd "$backup_dir" && sha256sum "$name" >"$name.sha256")
else # macOS
  (cd "$backup_dir" && shasum -a 256 "$name" >"$name.sha256")
fi
log "wrote $archive ($(du -h "$archive" | cut -f1))"

# days_since_epoch YYYYmmdd prints the day number of a date, using only
# shell arithmetic so it works the same with GNU and BSD date.
days_since_epoch() {
  local y=$((10#${1:0:4})) m=$((10#${1:4:2})) d=$((10#${1:6:2})) era yoe doy doe
  if ((m <= 2)); then y=$((y - 1)); fi
  era=$((y / 400))
  yoe=$((y - era * 400))
  doy=$(((153 * (m > 2 ? m - 3 : m + 9) + 2) / 5 + d - 1))
  doe=$((yoe * 365 + yoe / 4 - yoe / 100 + doy))
  echo $((era * 146097 + doe - 719468))
}

# Prune: walking from newest to oldest, keep the newest backup of each of
# the last keep_daily days and of each of the last keep_weekly weeks
# (Monday to Sunday). Only files named like this script's archives are
# touched.
daily=0 weekly=0 last_day="" last_week="" pruned=0
while IFS= read -r file; do
  day=${file#"$level"-}
  day=${day:0:8}
  week=$((($(days_since_epoch "$day") + 3) / 7)) # 1970-01-01 was a Thursday
  keep=false
  if [[ $day != "$last_day" ]]; then
    last_day=$day
    if ((daily < keep_daily)); then
      keep=true
      daily=$((daily + 1))
    fi
  fi
  if [[ $week != "$last_week" ]]; then
    last_week=$week
    if ((weekly < keep_weekly)); then
      keep=true
      weekly=$((weekly + 1))
    fi
  fi
  if ! $keep; then
    rm -f "$backup_dir/$file" "$backup_dir/$file.sha256"
    log "pruned $file"
    pruned=$((pruned + 1))
  fi
done < <(cd "$backup_dir" && find . -maxdepth 1 -type f -name "$level-*.tar.gz" | sed 's|^\./||' |
  grep -E "^$level-[0-9]{8}-[0-9]{6}\.tar\.gz$" | sort -r)
log "kept $daily daily and $weekly weekly backups, pruned $pruned"

if [[ -n $copy_to ]]; then
  # Failing here still leaves the local backup in place.
  rsync -a "$archive" "$archive.sha256" "$copy_to" || die "copy to $copy_to failed; the local backup is fine"
  log "copied to $copy_to"
fi
