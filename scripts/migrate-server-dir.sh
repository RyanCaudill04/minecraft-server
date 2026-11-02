#!/usr/bin/env bash
# Moves the Minecraft server's runtime files out of the git checkout into
# their own server folder (SERVER_DIR), so the checkout only holds code and
# config, and deleting or re-cloning it can't touch the world (#73).
#
# Usage: scripts/migrate-server-dir.sh [--checkout DIR] [--server-dir DIR]
#          [--move-checkout-to DIR] [--backup-dir DIR | --no-backup] [--apply]
#
# Dry run by default: it prints the plan and changes nothing. --apply does it.
#
# The recommended move on the Pi keeps the server folder where it is and
# moves the checkout instead, so every unit, play's -server flag and the
# backup script keep their ~/minecraft_server default:
#
#   scripts/migrate-server-dir.sh --move-checkout-to ~/src/minecraft-server
#
# That renames ~/minecraft_server to ~/src/minecraft-server, then moves the
# runtime files back into a fresh ~/minecraft_server. Each move is a rename
# on the same disk, so the world is never copied.
#
# What goes where. Every top-level entry of the checkout is one of:
#   keep  repo-only (code, docs, deploy files): stays in the checkout
#   move  not tracked by git at all (world*/, logs/, libraries/, cache/,
#         lazymc, server.properties, usercache.json, ...): moved
#   copy  tracked by git (start.sh, lazymc.toml, bukkit.yml, config/,
#         plugins/, server.jar, ops.json, ...): copied with its untracked
#         files, which are then removed from the checkout. The live copy is
#         the one that counts from now on; the checkout keeps git's.
#
# Defaults: --checkout is this script's checkout; --server-dir is
# $SERVER_DIR, else ~/minecraft_server. With no options that's the same
# folder as the checkout on the Pi, which is today's layout: nothing to do.
#
# A server folder other than ~/minecraft_server needs a minecraft.service
# that reads SERVER_DIR (#107); until then --server-dir refuses one.
#
# --apply refuses to run while minecraft.service, LazyMC or Paper is
# running, or if the server folder already has files in it. Before moving
# anything it writes a backup of the world (level-name in
# server.properties, and any other folder with a level.dat) and the
# server's own files to --backup-dir (default ~/backups/minecraft) and
# checks it can be read.
#
# If --apply stops partway (a full disk, a permission problem, Ctrl-C), it
# prints what's left, and the server folder keeps a .migrate-in-progress
# note. Running it again with --checkout set to where the checkout is now
# picks up where it stopped.
set -euo pipefail

# Entries that belong to the repo, not the server. Anything else at the
# top of the checkout is the server's.
repo_only=(.git .github .gitignore .claude README.md LICENSE deploy scripts sites web
  docs bin connection.private.md server.properties.example)

usage() { sed -n '6,7p' "$0" | sed 's/^# \{0,1\}//'; }
bad() { echo "migrate-server-dir: $*" >&2; usage >&2; exit 2; }
die() { echo "migrate-server-dir: $*" >&2; exit 1; }

# abs prints a path made absolute, without needing it to exist.
abs() {
  case $1 in
    /*) printf '%s\n' "${1%/}" ;;
    *) printf '%s/%s\n' "$(pwd)" "${1%/}" ;;
  esac
}

# canon prints a path made absolute with symlinks, "." and repeated
# slashes resolved, so two spellings of one folder compare equal. Parts
# that don't exist yet are kept as given.
canon() {
  local p parent
  p=$(abs "$1")
  if [ -d "$p" ]; then
    (cd -P "$p" && pwd -P)
  else
    parent=$(canon "$(dirname "$p")")
    printf '%s/%s\n' "${parent%/}" "$(basename "$p")"
  fi
}

# level_name prints server.properties' level-name in folder $1, or world.
level_name() {
  local level=''
  [ ! -f "$1/server.properties" ] ||
    level=$(sed -n 's/^level-name=//p' "$1/server.properties" | tail -n 1 | tr -d '\r')
  printf '%s\n' "${level:-world}"
}

marker_name=.migrate-in-progress

is_repo_only() {
  local e
  for e in "${repo_only[@]}"; do [ "$1" = "$e" ] && return 0; done
  return 1
}

# The whole script is read before main runs, so it keeps working after
# --move-checkout-to renames the folder it lives in.
main() {
  local checkout='' server_dir=${SERVER_DIR:-$HOME/minecraft_server} new_checkout=''
  local backup_dir=$HOME/backups/minecraft backup=true apply=false self
  checkout=$(cd "$(dirname "$0")/.." && pwd)
  # This script, for the finish command if --apply stops partway. On the
  # Pi it's usually a copy outside the checkout, whose scripts/ isn't
  # pulled and may not have it.
  self=$(canon "$0")

  while [ $# -gt 0 ]; do
    case $1 in
      --checkout | --server-dir | --move-checkout-to | --backup-dir)
        [ $# -ge 2 ] || bad "$1 needs a value"
        case $1 in
          --checkout) checkout=$2 ;;
          --server-dir) server_dir=$2 ;;
          --move-checkout-to) new_checkout=$2 ;;
          --backup-dir) backup_dir=$2 ;;
        esac
        shift 2
        ;;
      --no-backup) backup=false; shift ;;
      --apply) apply=true; shift ;;
      -h | --help) usage; exit 0 ;;
      *) bad "unknown argument $1" ;;
    esac
  done

  [ -d "$checkout/.git" ] || [ -f "$checkout/.git" ] || die "$checkout isn't a git checkout"
  checkout=$(cd -P "$checkout" && pwd -P)
  server_dir=$(canon "$server_dir")
  [ -z "$new_checkout" ] || new_checkout=$(canon "$new_checkout")
  [[ $server_dir$new_checkout != *[[:space:]]* ]] || die "paths with spaces aren't supported"
  local home_server
  home_server=$(canon "$HOME/minecraft_server")

  # Where the checkout's files are when the moves run.
  local src=$checkout
  if [ -n "$new_checkout" ]; then
    [ ! -e "$new_checkout" ] || die "--move-checkout-to $new_checkout already exists"
    case $new_checkout/ in "$checkout"/*) die "--move-checkout-to can't be inside the checkout" ;; esac
    src=$new_checkout
  elif [ "$server_dir" = "$checkout" ]; then
    echo "The server folder is the checkout ($checkout): today's layout."
    echo "Pass --server-dir DIR, or --move-checkout-to DIR to keep the server folder here."
    return 0
  fi
  case $server_dir/ in "$src"/*) die "the server folder can't be inside the checkout" ;; esac

  # A run that stopped partway left a note: line 1 is where the checkout
  # was, line 2 where it went. Only that checkout may finish it.
  local marker=$server_dir/$marker_name resume=false old_checkout=$checkout
  if [ "$server_dir" != "$checkout" ] && [ -f "$marker" ]; then
    [ -z "$new_checkout" ] || die "$server_dir has an unfinished migration; run again with --checkout $(sed -n 2p "$marker")"
    [ "$(sed -n 2p "$marker")" = "$checkout" ] ||
      die "$server_dir has an unfinished migration from $(sed -n 2p "$marker"), not $checkout"
    resume=true
    old_checkout=$(sed -n 1p "$marker")
    echo "Resuming the migration that stopped partway (see $marker)."
  elif [ "$server_dir" != "$checkout" ] && [ -e "$server_dir" ] &&
    [ -n "$(find "$server_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
    die "$server_dir already has files in it; this only moves into a new or empty folder"
  fi

  # minecraft.service on main runs ~/minecraft_server/lazymc; #107's reads
  # SERVER_DIR from host.env.
  local unit_link=$HOME/.config/systemd/user/minecraft.service
  if [ "$server_dir" != "$home_server" ] && ! grep -qs 'SERVER_DIR' "$unit_link"; then
    die "minecraft.service runs ~/minecraft_server, and the installed unit doesn't read SERVER_DIR (#107); use --move-checkout-to to keep the server folder at ~/minecraft_server"
  fi

  # Sort each top-level entry into keep, move or copy.
  local keep=() move=() copy=() name
  while IFS= read -r name; do
    name=${name#./}
    if is_repo_only "$name"; then
      keep+=("$name")
    elif [ -n "$(git -C "$checkout" ls-files -- "$name" | head -n 1)" ]; then
      copy+=("$name")
    else
      move+=("$name")
    fi
  done < <(cd "$checkout" && find . -mindepth 1 -maxdepth 1 | LC_ALL=C sort)

  echo "Checkout:      $checkout"
  [ -z "$new_checkout" ] || echo "  moves to:    $new_checkout"
  echo "Server folder: $server_dir"
  echo
  printf '  keep  %s\n' "${keep[@]}"
  [ ${#move[@]} = 0 ] || printf '  move  %s\n' "${move[@]}"
  [ ${#copy[@]} = 0 ] || printf '  copy  %s\n' "${copy[@]}"
  echo

  # The unit links into the checkout, so it moves with it.
  local unit_target='' unit_new=''
  if [ "$src" != "$old_checkout" ] && [ -L "$unit_link" ]; then
    unit_target=$(canon "$(readlink "$unit_link")")
    case $unit_target in
      "$old_checkout"/*)
        unit_new=$src/${unit_target#"$old_checkout"/}
        echo "  relink $unit_link -> $unit_new"
        ;;
    esac
  fi
  if [ ${#move[@]} = 0 ] && [ ${#copy[@]} = 0 ]; then
    echo "Nothing to move."
  fi

  if ! $apply; then
    echo "Dry run: nothing was changed. Stop the server, then run again with --apply."
    return 0
  fi

  # --- From here on, files change.
  if command -v systemctl >/dev/null && systemctl --user is-active --quiet minecraft.service 2>/dev/null; then
    die "minecraft.service is running; stop it first: systemctl --user stop minecraft"
  fi
  # Any LazyMC or Paper on this machine, including one started by hand.
  if command -v pgrep >/dev/null; then
    ! pgrep -x lazymc >/dev/null || die "LazyMC is running; stop it first"
    ! pgrep -f org.bukkit.craftbukkit.Main >/dev/null || die "Paper is running; stop it first"
  else
    echo "warning: no pgrep, so this can't check that LazyMC and Paper are stopped" >&2
  fi

  # The worlds: level-name's three, and any other folder with a level.dat.
  # Nothing has moved yet, so the files are still in $checkout.
  local level worlds=() f
  level=$(level_name "$checkout")
  for f in "${move[@]}"; do
    case $f in
      "$level" | "${level}_nether" | "${level}_the_end") worlds+=("$f") ;;
      *) [ ! -f "$checkout/$f/level.dat" ] || worlds+=("$f") ;;
    esac
  done
  for f in "${worlds[@]}"; do
    if [ -e "$checkout/$f/session.lock" ] && command -v fuser >/dev/null &&
      fuser "$checkout/$f/session.lock" >/dev/null 2>&1; then
      die "something has $checkout/$f/session.lock open; stop Paper first"
    fi
  done
  if ! $resume && [ ! -d "$checkout/$level" ]; then
    echo "warning: server.properties says level-name=$level, but there's no $level folder" >&2
  fi

  if $resume; then
    echo "Not backing up again: the first run did, before it moved anything (see $backup_dir)."
  elif $backup; then
    # The worlds and the files the server owns: what can't be re-made.
    local stamp archive saved=()
    stamp=$(date +%Y%m%d-%H%M%S)
    archive=$backup_dir/pre-migrate-$stamp.tar.gz
    saved=("${worlds[@]}")
    for f in server.properties usercache.json ops.json whitelist.json banned-ips.json banned-players.json; do
      [ ! -e "$checkout/$f" ] || saved+=("$f")
    done
    if [ ${#saved[@]} -gt 0 ]; then
      mkdir -p "$backup_dir"
      echo "Backing up ${saved[*]} to $archive"
      (umask 077 && tar -czf "$archive.partial" -C "$checkout" "${saved[@]}")
      tar -tzf "$archive.partial" >/dev/null || die "the backup $archive.partial can't be read; nothing was moved"
      mv "$archive.partial" "$archive"
    else
      echo "No world or server files to back up."
    fi
  fi

  # If anything below fails, say how to finish. Nothing is deleted before
  # it's in the server folder, so a re-run only has what's left to do.
  local rerun=$self
  case $self in "$checkout"/*) rerun=$src/${self#"$checkout"/} ;; esac
  # shellcheck disable=SC2064 # expand now: these locals are gone at exit
  trap "on_exit '$rerun' '$src' '$server_dir'" EXIT

  if [ -n "$new_checkout" ]; then
    mkdir -p "$(dirname "$new_checkout")"
    echo "+ mv $checkout $new_checkout"
    mv "$checkout" "$new_checkout"
  fi
  mkdir -p "$server_dir"
  printf '%s\n%s\n' "$old_checkout" "$src" >"$marker"

  for name in "${move[@]}"; do
    [ ! -e "$server_dir/$name" ] || die "both $src/$name and $server_dir/$name exist; move one aside, then run again"
    echo "+ mv $src/$name $server_dir/"
    mv "$src/$name" "$server_dir/"
  done
  for name in "${copy[@]}"; do
    echo "+ cp -Rp $src/$name $server_dir/"
    cp -Rp "$src/$name" "$server_dir/"
    # Only git's files stay in the checkout; the rest now live in the
    # server folder. -x removes ignored files too, but only under $name.
    if [ -d "$src/$name" ]; then
      git -C "$src" clean -fdxq -- "$name"
    fi
  done

  if [ -n "$unit_new" ]; then
    ln -sfn "$unit_new" "$unit_link"
    echo "+ relinked $unit_link"
  fi
  rm -f "$marker"
  trap - EXIT

  echo
  echo "Done. Next:"
  [ -z "$unit_new" ] || echo "  systemctl --user daemon-reload"
  if [ "$server_dir" != "$home_server" ]; then
    echo "  set SERVER_DIR=$server_dir in ~/.config/minecraft/host.env (the unit defaults to ~/minecraft_server)"
  fi
  echo "  systemctl --user start minecraft, then join and check the world"
  local dirty line
  dirty=$(git -C "$src" status --porcelain)
  if [ -n "$dirty" ]; then
    echo "  the checkout still differs from git (the server's live copies are in $server_dir now):"
    while IFS= read -r line; do echo "    $line"; done <<<"$dirty"
    echo "  reset it with: git -C $src checkout -- . (only after checking the server folder)"
  fi
}

# on_exit <script> <checkout> <server folder> runs if --apply stops partway.
on_exit() {
  {
    echo
    echo "migrate-server-dir: stopped partway. Nothing was deleted, and the backup is intact."
    echo "Fix the error above, then finish with:"
    echo "  bash $1 --checkout $2 --server-dir $3 --apply"
  } >&2
}

main "$@"
