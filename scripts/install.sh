#!/usr/bin/env bash
# Sets up this machine to run the Minecraft server and the web apps from
# this clone, under systemd user services.
#
# Usage: scripts/install.sh --repo owner/name --domain example.com
#          [--server-dir DIR] [--heap 4G] [--accept-eula] [--yes] [--dry-run]
#
# --repo and --domain are needed the first time; later runs read them from
# ~/.config/minecraft/host.env. Safe to re-run: each step checks first and
# changes only what's missing, and asks before replacing anything (--yes
# answers yes). --dry-run prints what it would do and changes nothing.
# It never runs sudo: if something needs root, it prints the command.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
config=$HOME/.config/minecraft
host_env=$config/host.env
secrets_env=$config/secrets.env
unit_dir=$HOME/.config/systemd/user
apps=$HOME/apps
java_min=25 # Paper 26.2 is built for Java 25
user=${USER:-$(id -un)}

repo='' domain='' server_dir='' heap=''
accept_eula=false yes=false dry_run=false

usage() { sed -n '5,6p' "$0" | sed 's/^# \{0,1\}//'; }
bad() { echo "install: $*" >&2; usage >&2; exit 2; }
die() { echo "install: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case $1 in
    --repo | --domain | --server-dir | --heap)
      [ $# -ge 2 ] || bad "$1 needs a value"
      case $1 in
        --repo) repo=$2 ;;
        --domain) domain=$2 ;;
        --server-dir) server_dir=$2 ;;
        --heap) heap=$2 ;;
      esac
      shift 2
      ;;
    --accept-eula) accept_eula=true; shift ;;
    --yes) yes=true; shift ;;
    --dry-run) dry_run=true; shift ;;
    -h | --help) usage; exit 0 ;;
    *) bad "unknown argument $1" ;;
  esac
done

[ "$(id -u)" != 0 ] || die "run this as the user the server runs as, not root"

todo=() # steps left for the owner, printed at the end
step() { printf '\n== %s\n' "$*"; }
ok() { echo "  ok: $*"; }
note() { echo "  todo: $*"; todo+=("$*"); }

# run prints a command, then runs it unless this is a dry run.
run() {
  if $dry_run; then echo "  would run: $*"; else echo "  + $*"; "$@"; fi
}

# confirm asks a yes/no question; no means leave things as they are.
confirm() {
  if $dry_run; then echo "  would ask: $1 [y/N]"; return 1; fi
  if $yes; then echo "  $1 yes (--yes)"; return 0; fi
  if [ ! -t 0 ]; then echo "  $1 no (no terminal to ask; re-run with --yes)"; return 1; fi
  local answer
  read -r -p "  $1 [y/N] " answer
  [[ $answer == [yY]* ]]
}

# get_key prints the value of key in an env-style file, or nothing.
get_key() {
  [ -f "$1" ] || return 0
  sed -n "s/^$2=//p" "$1" | tail -n 1
}

# set_key sets key=value in file, in place, keeping its other lines and
# its mode.
set_key() {
  local file=$1 key=$2 value=$3 tmp
  if grep -q "^$key=" "$file" 2>/dev/null; then
    tmp=$(mktemp "$file.XXXXXX")
    awk -v k="$key" -v v="$value" 'index($0, k "=") == 1 { print k "=" v; next } { print }' "$file" >"$tmp"
    cat "$tmp" >"$file"
    rm -f "$tmp"
  else
    if [ -s "$file" ] && [ -n "$(tail -c 1 "$file")" ]; then echo >>"$file"; fi
    printf '%s=%s\n' "$key" "$value" >>"$file"
  fi
}

# ensure_key makes key=value in file, asking before it changes a value
# that's already set. Returns 0 if the file now has (or would have) it.
host_changed=false
ensure_key() {
  local file=$1 key=$2 value=$3 old
  old=$(get_key "$file" "$key")
  if grep -q "^$key=" "$file" 2>/dev/null; then
    if [ "$old" = "$value" ]; then ok "$key=$value"; return 0; fi
    confirm "change $key from $old to $value?" || return 1
  fi
  if $dry_run; then echo "  would set $key=$value"; return 0; fi
  set_key "$file" "$key" "$value"
  echo "  set $key=$value"
  host_changed=true
}

# ---------------------------------------------------------------------------
step "Prerequisites"
missing=()
[ "$(uname -s)" = Linux ] || missing+=("Linux with systemd (this is $(uname -s))")
if ! command -v systemctl >/dev/null || ! systemctl --user show-environment >/dev/null 2>&1; then
  missing+=("the systemd user manager: systemctl --user must work (log in as $user directly, not with su)")
fi
if ! command -v loginctl >/dev/null || [ "$(loginctl show-user "$user" --property=Linger --value 2>/dev/null)" != yes ]; then
  missing+=("lingering, so the services run while you're logged out: sudo loginctl enable-linger $user")
fi
if command -v java >/dev/null; then
  java_version=$(java -version 2>&1 | sed -nE '1s/.*version "([0-9]+).*/\1/p')
  [ "${java_version:-0}" -ge "$java_min" ] ||
    missing+=("Java $java_min or newer (found ${java_version:-an unknown version}), e.g. sudo apt install openjdk-$java_min-jre-headless")
else
  missing+=("Java $java_min or newer, e.g. sudo apt install openjdk-$java_min-jre-headless")
fi
for cmd in curl tar sha256sum awk; do
  command -v "$cmd" >/dev/null || missing+=("$cmd")
done
if [ ${#missing[@]} -gt 0 ]; then
  echo "  missing:"
  printf '    - %s\n' "${missing[@]}"
  $dry_run || die "install the above, then run this again"
  echo "  (continuing because of --dry-run)"
else
  ok "Linux, systemd user manager, lingering, Java $java_version, curl, tar, sha256sum"
fi

case $(uname -m) in
  aarch64 | arm64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) arch=$(uname -m) ;;
esac

# ---------------------------------------------------------------------------
step "Host config ($host_env)"
repo=${repo:-$(get_key "$host_env" REPO)}
domain=${domain:-$(get_key "$host_env" DOMAIN)}
server_dir=${server_dir:-$(get_key "$host_env" SERVER_DIR)}
server_dir=${server_dir:-$root}
heap=${heap:-$(get_key "$host_env" HEAP)}
if [ -z "$heap" ]; then
  # About 60% of RAM, leaving the rest for the OS and the web apps.
  mem_kb=$(awk '/^MemTotal:/ { print $2 }' /proc/meminfo 2>/dev/null || true)
  [ -n "$mem_kb" ] || bad "can't read the RAM size from /proc/meminfo; pass --heap, e.g. --heap 4G"
  heap="$((mem_kb * 6 / 10 / 1024 / 1024))G"
  [ "$heap" != 0G ] || heap=1G
fi

[ -n "$repo" ] || bad "--repo owner/name is required the first time"
[ -n "$domain" ] || bad "--domain is required the first time"
[[ $repo =~ ^[A-Za-z0-9-]+/\.?[A-Za-z0-9_-][A-Za-z0-9_.-]*$ ]] || bad "--repo must be owner/name, not $repo"
[[ $domain =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]] ||
  bad "--domain must be a domain name like example.com, not $domain"
[[ $heap =~ ^[1-9][0-9]*[MG]$ ]] || bad "--heap must be a size like 4G or 4096M, not $heap"
[ -d "$server_dir" ] || bad "--server-dir $server_dir doesn't exist"
server_dir=$(cd "$server_dir" && pwd)
[[ $server_dir != *[[:space:]]* ]] || bad "--server-dir can't contain spaces: $server_dir"
# Until the server's files move out of the checkout (#73), the server
# folder is a checkout of this repo.
[ -f "$server_dir/start.sh" ] && [ -f "$server_dir/lazymc.toml" ] ||
  bad "$server_dir has no start.sh and lazymc.toml; for now the server folder is this checkout ($root)"

if [ ! -f "$host_env" ]; then
  if $dry_run; then
    echo "  would create $host_env"
  else
    mkdir -p "$config"
    printf '# Written by scripts/install.sh. Every key is described in\n# deploy/host.example.env.\n' >"$host_env"
    echo "  created $host_env"
  fi
fi
ensure_key "$host_env" REPO "$repo" || true
ensure_key "$host_env" DOMAIN "$domain" || true
ensure_key "$host_env" SERVER_DIR "$server_dir" || true
ensure_key "$host_env" HEAP "$heap" || true
play_addr=$(get_key "$host_env" PLAY_ADDR)
admin_addr=$(get_key "$host_env" ADMIN_ADDR)

# ---------------------------------------------------------------------------
step "Secrets ($secrets_env)"
if [ ! -f "$secrets_env" ]; then
  if $dry_run; then
    echo "  would create it from deploy/secrets.example.env with mode 0600"
  else
    mkdir -p "$config"
    (umask 077 && cp "$root/deploy/secrets.example.env" "$secrets_env")
    echo "  created it from deploy/secrets.example.env with mode 0600"
  fi
fi
if [ -f "$secrets_env" ]; then
  if [ -z "$(find "$secrets_env" -perm 0600)" ]; then
    run chmod 600 "$secrets_env"
  else
    ok "mode 0600"
  fi
fi
rcon_password=$(get_key "$secrets_env" RCON_PASSWORD)
if [ -n "$rcon_password" ]; then
  ok "RCON_PASSWORD is set"
elif $dry_run; then
  echo "  would generate RCON_PASSWORD"
else
  rcon_password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
  set_key "$secrets_env" RCON_PASSWORD "$rcon_password"
  echo "  generated RCON_PASSWORD"
fi
if [ -z "$(get_key "$secrets_env" ACCESS_AUD)" ]; then
  note "fill in ACCESS_TEAM_DOMAIN and ACCESS_AUD in $secrets_env, then: systemctl --user restart admin"
fi

# ---------------------------------------------------------------------------
step "Server files ($server_dir)"
server_ready=true
if [ -x "$server_dir/lazymc" ]; then
  ok "lazymc"
else
  lazymc_version=$(sed -nE 's/^version = "([0-9.]+)"$/\1/p' "$server_dir/lazymc.toml" | tail -n 1)
  note "download LazyMC ${lazymc_version:-} for Linux $arch from https://github.com/timvisee/lazymc/releases to $server_dir/lazymc and chmod +x it"
  server_ready=false
fi
if [ -d "$server_dir/libraries" ]; then
  ok "libraries/"
elif [ -f "$server_dir/server.jar" ]; then
  # Paperclip downloads Paper's libraries and stops, without starting it.
  if $dry_run; then
    echo "  would run in $server_dir: java -Dpaperclip.patchonly=true -jar server.jar"
  else
    echo "  + (cd $server_dir && java -Dpaperclip.patchonly=true -jar server.jar)"
    (cd "$server_dir" && java -Dpaperclip.patchonly=true -jar server.jar)
  fi
else
  note "put Paper's Paperclip jar in $server_dir/server.jar and run this again"
  server_ready=false
fi
if grep -qx 'eula=true' "$server_dir/eula.txt" 2>/dev/null; then
  ok "eula.txt accepts the Minecraft EULA"
elif $accept_eula; then
  if $dry_run; then echo "  would write eula=true to $server_dir/eula.txt"; else
    echo 'eula=true' >"$server_dir/eula.txt"
    echo "  wrote eula=true to $server_dir/eula.txt (--accept-eula)"
  fi
else
  note "read https://aka.ms/MinecraftEULA, then run this again with --accept-eula"
  server_ready=false
fi
properties=$server_dir/server.properties
if [ -f "$properties" ]; then
  ok "server.properties"
else
  run cp "$root/server.properties.example" "$properties"
fi
# The RCON password lives in secrets.env; server.properties gets a copy.
if [ -z "$rcon_password" ]; then
  echo "  would copy RCON_PASSWORD into server.properties (rcon.password)"
elif [ "$(get_key "$properties" rcon.password)" = "$rcon_password" ]; then
  ok "rcon.password matches secrets.env"
elif [ -z "$(get_key "$properties" rcon.password)" ] || confirm "replace rcon.password in server.properties with RCON_PASSWORD from secrets.env?"; then
  if $dry_run; then echo "  would copy RCON_PASSWORD into server.properties"; else
    set_key "$properties" rcon.password "$rcon_password"
    echo "  copied RCON_PASSWORD into server.properties (takes effect when Paper next starts)"
  fi
fi

# ---------------------------------------------------------------------------
step "Web apps ($apps)"
if [ -L "$apps/current" ]; then
  ok "release $(basename "$(readlink "$apps/current")") is active; mc-agent.timer installs new ones"
elif [ "$arch" != arm64 ]; then
  note "releases are built for arm64 only so far (#72), so the web apps can't be installed on $arch yet"
else
  # mc-agent.service runs mc-agent from ~/apps/current, which only exists
  # after a deploy. So run the newest release's mc-agent once from a temp
  # folder: it installs that release into ~/apps and enables its timer.
  url=https://github.com/$repo/releases/latest/download
  if $dry_run; then
    echo "  would download $url/release.tar.gz, check its SHA-256, unpack it to a temp folder and run its mc-agent deploy with REPO=$repo"
  else
    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    echo "  downloading $url/release.tar.gz"
    for asset in release.tar.gz release.tar.gz.sha256; do
      curl -fsSL -o "$tmp/$asset" "$url/$asset" ||
        die "no release to download from $repo yet; CI publishes one after a push to main"
    done
    (cd "$tmp" && sha256sum -c --quiet release.tar.gz.sha256) || die "release.tar.gz doesn't match its checksum"
    mkdir "$tmp/release"
    tar -xzf "$tmp/release.tar.gz" -C "$tmp/release"
    run env REPO="$repo" "$tmp/release/bin/mc-agent" deploy
  fi
fi

# ---------------------------------------------------------------------------
step "Minecraft service"
src=$root/deploy/systemd/minecraft.service
link=$unit_dir/minecraft.service
if [ -L "$link" ] && [ "$link" -ef "$src" ]; then
  ok "$link links to $src"
elif [ -e "$link" ] || [ -L "$link" ]; then
  if confirm "replace $link with a link to $src?"; then
    run rm -f "$link"
    run ln -s "$src" "$link"
  else
    note "$link isn't a link to $src; replace it to use this checkout's unit"
  fi
else
  run mkdir -p "$unit_dir"
  run ln -s "$src" "$link"
fi
if $server_ready; then
  run systemctl --user daemon-reload
  # Starts it if it isn't running; never restarts a running server.
  run systemctl --user enable --now minecraft.service
else
  note "start the server once the above is done: run this again, or systemctl --user enable --now minecraft"
fi
if $host_changed; then
  note "units read host.env when they start: systemctl --user restart play admin, and restart minecraft when nobody is online"
fi

# ---------------------------------------------------------------------------
step "Status"
for unit in minecraft.service play.service admin.service site@portfolio.service mc-agent.timer; do
  state=$(systemctl --user is-active "$unit" 2>/dev/null || true)
  printf '  %-24s %s\n' "$unit" "${state:-unknown}"
done

portfolio_addr=$(get_key "$root/deploy/sites/portfolio.env" ADDR)
cat <<EOF

== Outside this machine (once; see #74)
  Cloudflare Tunnel public hostnames:
    $domain -> http://${portfolio_addr:-127.0.0.1:8080}
    play.$domain -> http://${play_addr:-127.0.0.1:8082}
    admin.$domain -> http://${admin_addr:-127.0.0.1:8081}, with a Cloudflare Access application in front
  DNS: mc.$domain -> this network's public IP (DNS only, not proxied)
  Router: forward TCP 25565 to this machine
EOF

if [ ${#todo[@]} -gt 0 ]; then
  echo
  echo "== Still to do"
  printf '  - %s\n' "${todo[@]}"
fi
if $dry_run; then echo; echo "Dry run: nothing was changed."; fi
