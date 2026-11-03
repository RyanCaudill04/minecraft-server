#!/usr/bin/env bash
# Copies the world files play reads from the Pi (ssh host mc-pi) into
# <dest>, in the same layout as ~/minecraft_server, keeping modification
# times. Read-only on the Pi: it only runs tar to stdout there.
#
# The snapshot holds real player data. Keep <dest> outside the repo and
# delete it when you're done.
#
# Usage: .claude/skills/play-preview/snapshot.sh <dest>
set -euo pipefail

dest=${1:?usage: snapshot.sh <dest>}
mkdir -p "$dest"

files=(
  usercache.json
  world/level.dat
  world/dimensions/minecraft/overworld/data/minecraft/world_clocks.dat
  world/dimensions/minecraft/overworld/data/minecraft/weather.dat
  world/players/stats
  world/players/data
  world/players/advancements
)

# tar keeps modification times on extract, which play uses for "last seen".
# The remote tar skips missing files (play treats world_clocks.dat and
# weather.dat as optional) and files Paper rewrites mid-read during an
# autosave, warning instead of failing the whole snapshot. GNU tar exits 1
# for "some files differ" and 2 for fatal errors, so only 2 or more fails.
# shellcheck disable=SC2029 # the file list is expanded locally on purpose
ssh mc-pi "cd ~/minecraft_server && { tar --ignore-failed-read --warning=no-file-changed -cf - ${files[*]}; rc=\$?; [ \"\$rc\" -le 1 ]; }" |
  tar -xpf - -C "$dest"
echo "snapshot in $dest"
