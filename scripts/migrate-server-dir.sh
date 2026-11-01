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
# --apply refuses to run while minecraft.service or this server's LazyMC is
# running, or if the server folder already has files in it. Before moving
# anything it writes a backup of the world and the server's own files to
# --backup-dir (default ~/backups/minecraft) and checks it can be read.
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

is_repo_only() {
  local e
  for e in "${repo_only[@]}"; do [ "$1" = "$e" ] && return 0; done
  return 1
}

# The whole script is read before main runs, so it keeps working after
# --move-checkout-to renames the folder it lives in.
main() {
  local checkout='' server_dir=${SERVER_DIR:-$HOME/minecraft_server} new_checkout=''
  local backup_dir=$HOME/backups/minecraft backup=true apply=false
  checkout=$(cd "$(dirname "$0")/.." && pwd)

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
  checkout=$(cd "$checkout" && pwd)
  server_dir=$(abs "$server_dir")
  [ -z "$new_checkout" ] || new_checkout=$(abs "$new_checkout")
  [[ $server_dir$new_checkout != *[[:space:]]* ]] || die "paths with spaces aren't supported"

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
  if [ "$server_dir" != "$checkout" ] && [ -e "$server_dir" ] &&
    [ -n "$(find "$server_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
    die "$server_dir already has files in it; this only moves into a new or empty folder"
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

  local unit_link=$HOME/.config/systemd/user/minecraft.service unit_target=''
  if [ -n "$new_checkout" ] && [ -L "$unit_link" ]; then
    unit_target=$(readlink "$unit_link")
    case $unit_target in
      "$checkout"/*) echo "  relink $unit_link -> $new_checkout/${unit_target#"$checkout"/}" ;;
      *) unit_target='' ;;
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
  if command -v pgrep >/dev/null && pgrep -f "$checkout/lazymc" >/dev/null; then
    die "LazyMC from $checkout is running; stop it first"
  fi
  if [ -e "$checkout/world/session.lock" ] && command -v fuser >/dev/null &&
    fuser "$checkout/world/session.lock" >/dev/null 2>&1; then
    die "something has $checkout/world/session.lock open; stop Paper first"
  fi

  if $backup; then
    # The world and the files the server owns: what can't be re-made.
    local stamp archive saved=() f
    stamp=$(date +%Y%m%d-%H%M%S)
    archive=$backup_dir/pre-migrate-$stamp.tar.gz
    for f in "${move[@]}" ops.json whitelist.json banned-ips.json banned-players.json; do
      case $f in world* | server.properties | usercache.json | ops.json | whitelist.json | banned-*.json)
        [ -e "$checkout/$f" ] && saved+=("$f") ;;
      esac
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

  if [ -n "$new_checkout" ]; then
    mkdir -p "$(dirname "$new_checkout")"
    echo "+ mv $checkout $new_checkout"
    mv "$checkout" "$new_checkout"
  fi
  mkdir -p "$server_dir"

  for name in "${move[@]}"; do
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

  if [ -n "$unit_target" ]; then
    ln -sfn "$new_checkout/${unit_target#"$checkout"/}" "$unit_link"
    echo "+ relinked $unit_link"
  fi

  echo
  echo "Done. Next:"
  [ -z "$unit_target" ] || echo "  systemctl --user daemon-reload"
  if [ "$server_dir" != "$HOME/minecraft_server" ]; then
    echo "  set SERVER_DIR=$server_dir in ~/.config/minecraft/host.env (the units default to ~/minecraft_server)"
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

main "$@"
