#!/usr/bin/env bash
# Tests for install.sh. Each case runs it with a fresh HOME and server
# folder, and with stand-ins for uname, systemctl, loginctl, java and curl,
# so it runs the same on a Mac and in CI and touches nothing real.
#
# Usage: scripts/install_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$dir/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/install_test.XXXXXX")
trap 'rm -rf "$work"' EXIT
failures=0

fail() { echo "FAIL $name: $*"; failures=$((failures + 1)); }

# Stand-ins log their calls to $work/calls.
stubs=$work/stubs
mkdir -p "$stubs"
cat >"$stubs/uname" <<'EOF'
#!/bin/sh
case $1 in -m) echo aarch64 ;; *) echo Linux ;; esac
EOF
cat >"$stubs/systemctl" <<'EOF'
#!/bin/sh
echo "systemctl $*" >>"$CALLS"
case "$*" in *is-active*) echo active ;; esac
EOF
cat >"$stubs/loginctl" <<'EOF'
#!/bin/sh
echo "${LINGER:-yes}"
EOF
cat >"$stubs/java" <<'EOF'
#!/bin/sh
if [ "$1" = -version ]; then echo 'openjdk version "25.0.1" 2025-10-21' >&2; exit 0; fi
echo "java $*" >>"$CALLS"
mkdir libraries
EOF
# curl -fsSL -o <out> <url>: serves files from $RELEASE by name.
cat >"$stubs/curl" <<'EOF'
#!/bin/sh
echo "curl $*" >>"$CALLS"
cp "$RELEASE/$(basename "$4")" "$3"
EOF
if ! command -v sha256sum >/dev/null; then # macOS
  printf '#!/bin/sh\nexec shasum -a 256 "$@"\n' >"$stubs/sha256sum"
fi
chmod +x "$stubs"/*

# A release whose mc-agent records how it was run and activates itself.
release=$work/release
mkdir -p "$release/stage/bin"
cat >"$release/stage/bin/mc-agent" <<'EOF'
#!/bin/sh
echo "mc-agent REPO=$REPO $*" >>"$CALLS"
mkdir -p "$HOME/apps/releases/r1" && ln -s "$HOME/apps/releases/r1" "$HOME/apps/current"
EOF
chmod +x "$release/stage/bin/mc-agent"
tar -czf "$release/release.tar.gz" -C "$release/stage" .
(cd "$release" && PATH="$stubs:$PATH" sha256sum release.tar.gz >release.tar.gz.sha256)

# fresh gives each case a new HOME and server folder.
fresh() {
  name=$1
  case_dir=$work/case-$((++cases))
  home=$case_dir/home server=$case_dir/server
  mkdir -p "$home" "$server/libraries"
  cp "$root/start.sh" "$root/lazymc.toml" "$server/"
  printf '#!/bin/sh\n' >"$server/lazymc"
  chmod +x "$server/lazymc"
  echo 'eula=true' >"$server/eula.txt"
  : >"$case_dir/calls"
}
cases=0

# install runs install.sh for the current case; output goes to $case_dir/out.
install() {
  local status=0
  HOME=$home PATH="$stubs:$PATH" CALLS=$case_dir/calls RELEASE=$release \
    "$dir/install.sh" "$@" </dev/null >"$case_dir/out" 2>&1 || status=$?
  return "$status"
}

key() { sed -n "s/^$2=//p" "$1" | tail -n 1; }
sums() { cat "$home/.config/minecraft/host.env" "$home/.config/minecraft/secrets.env" "$server/server.properties" | cksum; }

fresh "dry run changes nothing"
install --repo someone/fork --domain example.com --server-dir "$server" --heap 4G --dry-run ||
  fail "exit $?: $(cat "$case_dir/out")"
[ -z "$(find "$home" -mindepth 1)" ] || fail "wrote to HOME: $(find "$home" -mindepth 1)"
[ ! -e "$server/server.properties" ] || fail "wrote server.properties"
changes=$(grep -v 'show-environment\|is-active' "$case_dir/calls" || true)
[ -z "$changes" ] || fail "ran commands: $changes"
grep -q 'would run: ln -s' "$case_dir/out" || fail "doesn't say it would link minecraft.service"

fresh "first run sets everything up"
install --repo someone/fork --domain example.com --server-dir "$server" --heap 4G ||
  fail "exit $?: $(cat "$case_dir/out")"
host=$home/.config/minecraft/host.env secrets=$home/.config/minecraft/secrets.env
[ "$(key "$host" REPO)" = someone/fork ] || fail "REPO is $(key "$host" REPO)"
[ "$(key "$host" DOMAIN)" = example.com ] || fail "DOMAIN is $(key "$host" DOMAIN)"
[ "$(key "$host" SERVER_DIR)" = "$(cd "$server" && pwd)" ] || fail "SERVER_DIR is $(key "$host" SERVER_DIR)"
[ "$(key "$host" HEAP)" = 4G ] || fail "HEAP is $(key "$host" HEAP)"
[ -n "$(find "$secrets" -perm 0600)" ] || fail "secrets.env isn't mode 0600"
password=$(key "$secrets" RCON_PASSWORD)
[[ $password =~ ^[0-9a-f]{48}$ ]] || fail "RCON_PASSWORD is '$password'"
[ "$(key "$server/server.properties" rcon.password)" = "$password" ] || fail "rcon.password isn't RCON_PASSWORD"
[ "$home/.config/systemd/user/minecraft.service" -ef "$root/deploy/systemd/minecraft.service" ] ||
  fail "minecraft.service isn't linked"
grep -qx 'mc-agent REPO=someone/fork deploy' "$case_dir/calls" || fail "didn't bootstrap mc-agent: $(cat "$case_dir/calls")"
grep -qx 'systemctl --user enable --now minecraft.service' "$case_dir/calls" || fail "didn't start minecraft"
grep -q 'admin.example.com' "$case_dir/out" || fail "doesn't list the Cloudflare hostnames"

name="second run changes nothing"
before=$(sums)
: >"$case_dir/calls"
install || fail "exit $?: $(cat "$case_dir/out")"
[ "$(sums)" = "$before" ] || fail "changed host.env, secrets.env or server.properties"
grep -q '^curl\|^mc-agent' "$case_dir/calls" && fail "deployed again: $(cat "$case_dir/calls")"

name="changing a value needs a yes"
install --heap 8G || fail "exit $?"
[ "$(key "$host" HEAP)" = 4G ] || fail "changed HEAP without asking"
install --heap 8G --yes || fail "exit $?"
[ "$(key "$host" HEAP)" = 8G ] || fail "HEAP is $(key "$host" HEAP) after --yes"

fresh "a replaced rcon.password needs a yes"
cp "$root/server.properties.example" "$server/server.properties"
echo 'rcon.password=kept' >>"$server/server.properties"
install --repo someone/fork --domain example.com --server-dir "$server" --heap 4G || fail "exit $?"
[ "$(key "$server/server.properties" rcon.password)" = kept ] || fail "replaced rcon.password without asking"

fresh "no EULA, no start"
echo 'eula=false' >"$server/eula.txt"
install --repo someone/fork --domain example.com --server-dir "$server" --heap 4G || fail "exit $?"
grep -q 'enable --now minecraft' "$case_dir/calls" && fail "started minecraft without the EULA"
grep -q 'accept-eula' "$case_dir/out" || fail "doesn't say how to accept the EULA"
install --accept-eula || fail "exit $?"
grep -qx 'eula=true' "$server/eula.txt" || fail "--accept-eula didn't write eula.txt"

fresh "missing lingering stops it"
status=0
LINGER=no install --repo someone/fork --domain example.com --server-dir "$server" --heap 4G || status=$?
[ "$status" = 1 ] || fail "exit $status, want 1"
grep -q "sudo loginctl enable-linger" "$case_dir/out" || fail "doesn't print the sudo command"
[ -z "$(find "$home" -mindepth 1)" ] || fail "changed things before stopping"

for args in "--domain example.com --heap 4G" "--repo nope --domain example.com --heap 4G" \
  "--repo a/b --domain bad_domain --heap 4G" "--repo a/b --domain example.com --heap 4" "--bogus"; do
  fresh "bad arguments: $args"
  status=0
  # shellcheck disable=SC2086 # split args on purpose
  install $args --server-dir "$server" || status=$?
  [ "$status" = 2 ] || fail "exit $status, want 2"
done

if [ "$failures" -gt 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "install.sh: all $cases cases passed"
