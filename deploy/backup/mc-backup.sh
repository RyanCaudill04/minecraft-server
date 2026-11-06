#!/usr/bin/env bash
# Backs up the Minecraft world to a timestamped .tar.gz, checks the archive,
# and removes backups older than BACKUP_KEEP_DAYS unless they're kept. Run nightly by mc-backup.timer; safe to run by hand.
#
# Getting a consistent copy:
#   - Paper asleep (LazyMC stopped it, or froze it with SIGSTOP, which is
#     how LazyMC sleeps it by default): nothing writes the world, so it is
#     copied as-is. If Paper starts or wakes during the copy, the copy is
#     retried.
#   - Paper awake, RCON enabled in server.properties: "save-off" and
#     "save-all flush" over RCON, copy, then "save-on" (always, even on
#     failure).
#   - Paper awake, no RCON: wait up to BACKUP_WAIT seconds for it to
#     sleep, then give up. A copy of region files Paper is writing can hold
#     torn chunks, and pruning would count it as a good backup.
#
# The world is copied to a staging folder first, so saving is paused only
# for the copy, not for the compression.
#
# Settings come from the environment; the systemd unit loads
# ~/.config/minecraft/host.env, the host settings file shared with the other
# services. Each BACKUP_* name also works with an MC_ prefix, which wins:
#   SERVER_DIR             server folder (default ~/minecraft_server; MC_SERVER_DIR wins)
#   BACKUP_DIR             where archives go (default ~/backups/minecraft)
#   BACKUP_KEEP_DAYS       remove backups this many days old (default 10), except
#                          ones kept for safe keeping (a <archive>.keep file,
#                          which admin's Backups section adds and removes)
#   BACKUP_WAIT            seconds to wait for Paper to sleep without RCON (default 1800)
#   BACKUP_COPY_TO         optional rsync destination for an off-Pi copy, e.g.
#                          user@nas:backups/minecraft/ (new archives only; it is
#                          never pruned by this script)
#   BACKUP_MIN_FREE_KB     spare space to leave on top of two copies of the world (default 1048576, 1 GiB)
# Other (environment only):
#   MC_RCON                mc-rcon binary (default: bin/mc-rcon of this release)
#   MC_PAPER_PATTERN       pgrep -f pattern that means Paper is running
#   MC_BACKUP_TIME         timestamp to use, YYYYmmdd-HHMMSS (for tests)
# The RCON password is read from server.properties, not from a secrets file.
#
# Archives are named <level-name>-YYYYmmdd-HHMMSS.tar.gz, with a .sha256 file
# next to each. Restore: see README.md.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
server_dir=${MC_SERVER_DIR:-${SERVER_DIR:-$HOME/minecraft_server}}
backup_dir=${MC_BACKUP_DIR:-${BACKUP_DIR:-$HOME/backups/minecraft}}
keep_days=${MC_BACKUP_KEEP_DAYS:-${BACKUP_KEEP_DAYS:-10}}
wait_secs=${MC_BACKUP_WAIT:-${BACKUP_WAIT:-1800}}
margin_kb=${MC_BACKUP_MIN_FREE_KB:-${BACKUP_MIN_FREE_KB:-1048576}}
copy_to=${MC_BACKUP_COPY_TO:-${BACKUP_COPY_TO:-}}
rcon_bin=${MC_RCON:-$here/../../bin/mc-rcon}
paper_pattern=${MC_PAPER_PATTERN:-org.bukkit.craftbukkit.Main}
stamp=${MC_BACKUP_TIME:-$(date +%Y%m%d-%H%M%S)}

log() { echo "mc-backup: $*"; }
die() { echo "mc-backup: $*" >&2; exit 1; }

for n in "$keep_days" "$wait_secs" "$margin_kb"; do
  [[ $n =~ ^[0-9]+$ ]] || die "retention and wait settings must be whole numbers, got '$n'"
done
((keep_days >= 1)) || die "BACKUP_KEEP_DAYS must be at least 1"
if [[ -n ${MC_BACKUP_KEEP_DAILY:-}${BACKUP_KEEP_DAILY:-}${MC_BACKUP_KEEP_WEEKLY:-}${BACKUP_KEEP_WEEKLY:-} ]]; then
  log "warning: BACKUP_KEEP_DAILY and BACKUP_KEEP_WEEKLY are no longer used; backups last BACKUP_KEEP_DAYS ($keep_days) days unless kept"
fi
[[ $stamp =~ ^[0-9]{8}-[0-9]{6}$ ]] || die "timestamp '$stamp' is not YYYYmmdd-HHMMSS"

props=$server_dir/server.properties
[[ -f $props ]] || die "$props not found; set SERVER_DIR"

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

# A run killed outright (SIGKILL, power loss) skips its exit trap and leaves
# a full-size .partial that nothing else would ever remove. The lock means
# no other run is writing one.
find "$backup_dir" -maxdepth 1 -type f -name "$level-*.tar.gz.partial" -delete

name=$level-$stamp.tar.gz
archive=$backup_dir/$name
[[ ! -e $archive ]] || die "$archive already exists"
staging=$backup_dir/.staging
partial=$archive.partial
saving_off=false

# The backup needs the world again in staging plus the archive, on the same
# disk Paper writes to. Refuse early rather than fill it.
need_kb=$(cd "$server_dir" && du -sk "${sources[@]}" | awk '{ n += $1 } END { print n + 0 }')
free_kb=$(df -Pk "$backup_dir" | awk 'NR == 2 { print $4 }')
((free_kb >= 2 * need_kb + margin_kb)) ||
  die "not enough free space in $backup_dir: ${free_kb} KB free, need about $((2 * need_kb + margin_kb)) KB (2x the world plus a margin)"

# rcon runs one command. mc-rcon reads the password from RCON_PASSWORD, so
# it never shows up in ps; -timeout leaves room for "save-all flush" on a
# large world.
rcon() { RCON_PASSWORD=$rcon_password "$rcon_bin" -addr "$rcon_addr" -timeout 120s "$@"; }

# resume_saving sends save-on if saving is off. It fails if save-on does,
# and then saving stays marked off so the exit trap tries again.
resume_saving() {
  if $saving_off; then
    rcon save-on >/dev/null || return 1
    saving_off=false
  fi
}

cleanup() {
  # Free the disk first: save-on lets Paper write again.
  rm -rf "$staging" "$partial"
  resume_saving || log "warning: save-on failed (fine if Paper has since stopped)"
}
trap cleanup EXIT

# paper_pids prints the PIDs of Paper processes that are awake, on one
# line, so two calls are equal only if the same process kept running in
# between. A frozen Paper (state T: LazyMC's freeze_process, the default)
# is asleep: it writes nothing, and it can't answer RCON.
paper_pids() {
  local pid
  for pid in $(pgrep -f "$paper_pattern" || true); do
    [[ $(ps -o stat= -p "$pid" 2>/dev/null | tr -d ' ') == T* ]] || printf '%s ' "$pid"
  done
}

paper_running() { [[ -n $(paper_pids) ]]; }

# copy_world copies the sources into the staging folder, taking care that
# Paper isn't writing them. It returns 1 if the copy has to be retried.
# It runs as an "until" condition, where set -e is off, so every step that
# can fail checks for it.
copy_world() {
  local mode=asleep pids=
  { rm -rf "$staging" && mkdir -p "$staging"; } || die "can't create $staging"

  if paper_running; then
    if [[ -n $rcon_password ]]; then
      mode=rcon
      log "Paper is running; pausing saves over RCON"
      # Set first: if save-off works but the flush fails, the exit trap
      # still turns saving back on.
      pids=$(paper_pids)
      saving_off=true
      if ! { rcon save-off >/dev/null && rcon save-all flush >/dev/null; }; then
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
    # Saving was only off for as long as this same Paper process lived: if
    # LazyMC stopped it (it writes the world on the way out) or started a
    # new one (saving is on again there), the copy isn't consistent.
    if [[ $(paper_pids) != "$pids" ]]; then
      log "Paper restarted or stopped during the copy; retrying"
      resume_saving || true
      return 1
    fi
    if ! resume_saving; then
      log "save-on failed; retrying"
      return 1
    fi
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

# stamp_secs YYYYmmdd-HHMMSS prints seconds since the epoch for a local
# timestamp, counting wall-clock time the way admin does (no DST shift).
stamp_secs() {
  local t=$1
  echo $(($(days_since_epoch "${t:0:8}") * 86400 + 10#${t:9:2} * 3600 + 10#${t:11:2} * 60 + 10#${t:13:2}))
}

# Clean up: remove every backup that is keep_days old or older, except ones
# kept for safe keeping and the one just written. Only files named like
# this script's archives are touched.
now=$(stamp_secs "$stamp")
max_age=$((keep_days * 86400))
kept=0 pruned=0
while IFS= read -r file; do
  t=${file%.tar.gz}
  t=${t: -15}
  if [[ $file == "$name" || -e $backup_dir/$file.keep ]] || ((now - $(stamp_secs "$t") < max_age)); then
    kept=$((kept + 1))
    continue
  fi
  rm -f "$backup_dir/$file" "$backup_dir/$file.sha256"
  log "removed $file (older than $keep_days days)"
  pruned=$((pruned + 1))
done < <(cd "$backup_dir" && find . -maxdepth 1 -type f -name "$level-*.tar.gz" | sed 's|^\./||' |
  grep -E "^$level-[0-9]{8}-[0-9]{6}\.tar\.gz$" | sort -r)
# A keep flag whose archive was removed by hand keeps nothing.
while IFS= read -r flag; do
  [[ -e $backup_dir/${flag%.keep} ]] || rm -f "$backup_dir/$flag"
done < <(cd "$backup_dir" && find . -maxdepth 1 -type f -name "$level-*.tar.gz.keep" | sed 's|^\./||')
log "kept $kept backups, removed $pruned older than $keep_days days"

if [[ -n $copy_to ]]; then
  # Failing here still leaves the local backup in place.
  rsync -a --timeout=300 -e 'ssh -o BatchMode=yes -o ConnectTimeout=30' "$archive" "$archive.sha256" "$copy_to" || die "copy to $copy_to failed; the local backup is fine"
  log "copied to $copy_to"
fi
