#!/usr/bin/env bash
# Finds, and with --apply removes, what the old manual deploy left on the
# Pi now that mc-agent runs the web apps from ~/apps/current (#17):
#
#   - ~/minecraft_server/bin/, the old serve/play binaries. Kept while any
#     user unit or running process still uses it.
#   - ~/minecraft_server/world.backup-* folders. Removed only with
#     --backups as well, once automated backups exist.
#   - user units that point into ~/minecraft_server. Only minecraft.service
#     should; others are listed for you to disable, never removed here.
#
# It only lists unless given --apply, and running it again after a cleanup
# finds nothing. Run it on the Pi, or from the Mac with
# "scripts/mc cleanup-legacy [--apply] [--backups]".
#
# Usage: pi-cleanup-legacy.sh [--apply] [--backups]
set -euo pipefail

server=${MC_SERVER_DIR:-$HOME/minecraft_server}
units=${MC_UNIT_DIR:-$HOME/.config/systemd/user}
apply=false backups=false

for arg in "$@"; do
  case $arg in
    --apply) apply=true ;;
    --backups) backups=true ;;
    -h | --help) sed -n '2,16s/^# \{0,1\}//p' "$0"; exit 0 ;;
    *) echo "pi-cleanup-legacy: unknown option $arg" >&2; exit 2 ;;
  esac
done

if "$apply"; then
  echo "Removing leftovers in $server"
else
  echo "Dry run: listing leftovers in $server (pass --apply to remove them)"
fi
found=0 blocked=0

# remove deletes path, or says it would.
remove() {
  found=$((found + 1))
  if "$apply"; then
    rm -rf -- "$1"
    echo "removed   $1"
  else
    echo "would remove $1 ($(du -sh -- "$1" 2>/dev/null | cut -f1))"
  fi
}

# Units other than minecraft.service that are links into the server
# folder or name a path inside it (comments aside).
name=$(basename "$server")
stray=()
if [ -d "$units" ]; then
  while IFS= read -r -d '' unit; do
    base=$(basename "$unit")
    [ "$base" = minecraft.service ] && continue
    target=$(readlink -f -- "$unit" 2>/dev/null || echo "$unit")
    if [[ $target == "$server"/* ]] || grep -v '^[[:space:]]*[#;]' "$unit" 2>/dev/null | grep -qF -- "$name/"; then
      stray+=("$unit")
    fi
  done < <(find "$units" -maxdepth 1 \( -type f -o -type l \) -print0 | sort -z)
fi
if [ ${#stray[@]} -eq 0 ]; then
  echo "ok        no unit but minecraft.service points into $server"
else
  for unit in ${stray[@]+"${stray[@]}"}; do
    found=$((found + 1))
    echo "check     $unit points into $server; if it's an old copy, run:"
    echo "            systemctl --user disable --now $(basename "$unit") && rm $unit"
  done
fi

# The old binaries, unless something still runs them.
bin=$server/bin
if [ -e "$bin" ]; then
  users=()
  for unit in ${stray[@]+"${stray[@]}"}; do
    grep -qF -- "$name/bin" "$unit" 2>/dev/null && users+=("$(basename "$unit")")
  done
  if command -v pgrep >/dev/null && pgrep -f -- "$bin/" >/dev/null; then
    users+=("a running process ($(pgrep -fl -- "$bin/" | head -n1))")
  fi
  if [ ${#users[@]} -gt 0 ]; then
    found=$((found + 1)) blocked=$((blocked + 1))
    echo "keep      $bin: still used by ${users[*]}"
  else
    remove "$bin"
  fi
fi

# Manual world backups.
for backup in "$server"/world.backup-*; do
  [ -d "$backup" ] || continue
  if "$backups"; then
    remove "$backup"
  else
    found=$((found + 1))
    echo "keep      $backup (add --backups to remove it, once automated backups exist)"
  fi
done

if [ "$found" -eq 0 ]; then
  echo "Nothing to clean up."
fi
if "$apply" && [ "$blocked" -gt 0 ]; then
  exit 1
fi
