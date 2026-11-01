#!/usr/bin/env bash
# Downloads the server files that deploy/server.json pins (Paper's
# Paperclip jar, LazyMC and each plugin jar) into a server folder, and
# checks every file's SHA-256.
#
# Usage: scripts/fetch-server.sh [--arch arm64|amd64] [--replace] [--patch]
#          [--manifest FILE] <server folder>
#
# Safe to re-run: a file that's already there with the right SHA-256 is
# left alone, and a missing one is downloaded, checked, then moved into
# place, so a failed download never leaves a half-written file behind.
# A file that's there with a different SHA-256 is an error, unless
# --replace is given: then the pinned file replaces it. That's how an
# upgrade goes: change the manifest, then run this with --replace.
#
# --patch also runs Paperclip once (java -Dpaperclip.patchonly=true), which
# downloads Mojang's server jar and Paper's libraries/ and writes the
# patched Paper jar that start.sh runs, then checks that jar's SHA-256.
# It needs the Java version the manifest names.
#
# --arch picks the LazyMC build; the default is this machine's.
# Needs curl, jq, and sha256sum or shasum.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
manifest=$root/deploy/server.json
arch='' replace=false patch=false target=''

usage() { sed -n '6,7p' "$0" | sed 's/^# \{0,1\}//'; }
bad() { echo "fetch-server: $*" >&2; usage >&2; exit 2; }
die() { echo "fetch-server: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case $1 in
    --arch | --manifest)
      [ $# -ge 2 ] || bad "$1 needs a value"
      if [ "$1" = --arch ]; then arch=$2; else manifest=$2; fi
      shift 2
      ;;
    --replace) replace=true; shift ;;
    --patch) patch=true; shift ;;
    -h | --help) usage; exit 0 ;;
    -*) bad "unknown option $1" ;;
    *)
      [ -z "$target" ] || bad "only one server folder, got $target and $1"
      target=$1
      shift
      ;;
  esac
done
[ -n "$target" ] || bad "which server folder?"
[ -d "$target" ] || die "$target isn't a folder"
[ -f "$manifest" ] || die "no manifest at $manifest"
for cmd in curl jq; do
  command -v "$cmd" >/dev/null || die "needs $cmd (e.g. sudo apt install $cmd)"
done

if [ -z "$arch" ]; then
  case $(uname -m) in
    aarch64 | arm64) arch=arm64 ;;
    x86_64 | amd64) arch=amd64 ;;
    *) die "no LazyMC build for $(uname -m); pass --arch arm64 or --arch amd64" ;;
  esac
fi

if command -v sha256sum >/dev/null; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
else # macOS
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
fi

# One line per file: path, URL, SHA-256 and mode, tab-separated.
rows=$(jq -r --arg arch "$arch" '
  def row(mode): [.path, .url, .sha256, mode] | @tsv;
  (.paper | row("644")),
  (.lazymc as $l
    | ($l.builds[$arch] // error("the manifest has no LazyMC build for \($arch)"))
    | .path = $l.path | row("755")),
  (.plugins[] | row("644"))
' "$manifest") || die "can't read $manifest"
summary=$(jq -r --arg arch "$arch" \
  '"Minecraft \(.minecraft), Paper build \(.paper.build), LazyMC \(.lazymc.version) (\($arch)), \(.plugins | length) plugins"' \
  "$manifest")
patched_path=$(jq -r '.paper.patched.path // empty' "$manifest")
patched_sha=$(jq -r '.paper.patched.sha256 // empty' "$manifest")
java_min=$(jq -r '.java // empty' "$manifest")

# A path must stay inside the server folder.
check_path() {
  case /$1/ in
    *//* | */../* | */./*) die "the manifest has an unsafe path: $1" ;;
  esac
}

failed=0
tmp=''
trap 'rm -f "$tmp"' EXIT

echo "$summary into $target"
while IFS=$'\t' read -r path url sha mode; do
  check_path "$path"
  [[ $sha =~ ^[0-9a-f]{64}$ ]] || die "the manifest has a bad SHA-256 for $path: $sha"
  [[ $url == https://* || $url == file://* ]] || die "the manifest has a non-https URL for $path: $url"
  dest=$target/$path

  if [ -f "$dest" ]; then
    have=$(sha256 "$dest")
    if [ "$have" = "$sha" ]; then
      echo "  ok       $path"
      continue
    fi
    if ! $replace; then
      echo "  MISMATCH $path: has $have, the manifest pins $sha (--replace to download the pinned file)" >&2
      failed=$((failed + 1))
      continue
    fi
    verb=replaced
  elif [ -e "$dest" ]; then
    echo "  ERROR    $path exists but isn't a file" >&2
    failed=$((failed + 1))
    continue
  else
    verb=fetched
  fi

  mkdir -p "$(dirname "$dest")"
  tmp=$(mktemp "$dest.download.XXXXXX")
  if ! curl -fsSL --retry 3 -o "$tmp" "$url"; then
    echo "  ERROR    $path: download failed from $url" >&2
    rm -f "$tmp"
    failed=$((failed + 1))
    continue
  fi
  have=$(sha256 "$tmp")
  if [ "$have" != "$sha" ]; then
    echo "  ERROR    $path: $url has SHA-256 $have, the manifest pins $sha; not installed" >&2
    rm -f "$tmp"
    failed=$((failed + 1))
    continue
  fi
  chmod "$mode" "$tmp"
  mv -f "$tmp" "$dest"
  tmp=''
  echo "  $verb  $path"
done <<<"$rows"

# Paper loads every jar in plugins/, so an old version left next to a new
# one would load twice. Point it out; it may also be a plugin added by hand.
for jar in "$target"/plugins/*.jar; do
  [ -f "$jar" ] || continue
  rel=plugins/$(basename "$jar")
  if ! cut -f1 <<<"$rows" | grep -qxF "$rel"; then
    echo "  warning  $rel isn't in the manifest; Paper still loads it (remove it if it's an old version)" >&2
  fi
done

# The patched Paper jar is made from server.jar by Paperclip, not
# downloaded. Check it if it's there; make it with --patch.
if [ -n "$patched_path" ] && [ "$failed" = 0 ]; then
  check_path "$patched_path"
  patched=$target/$patched_path
  have=''
  [ ! -f "$patched" ] || have=$(sha256 "$patched")
  if [ "$have" = "$patched_sha" ]; then
    echo "  ok       $patched_path"
  elif [ -n "$have" ] && ! $replace; then
    echo "  MISMATCH $patched_path: has $have, the manifest pins $patched_sha (--patch --replace to re-run Paperclip)" >&2
    failed=$((failed + 1))
  elif ! $patch; then
    echo "  missing  $patched_path: run this again with --patch (needs Java ${java_min:-}), or start.sh can't run"
  else
    command -v java >/dev/null || die "--patch needs Java ${java_min:-}"
    echo "  running  Paperclip (java -Dpaperclip.patchonly=true -jar server.jar)"
    (cd "$target" && java -Dpaperclip.patchonly=true -jar server.jar) || die "Paperclip failed"
    if [ -f "$patched" ] && [ "$(sha256 "$patched")" = "$patched_sha" ]; then
      echo "  patched  $patched_path"
    else
      echo "  ERROR    Paperclip didn't write $patched_path with SHA-256 $patched_sha" >&2
      failed=$((failed + 1))
    fi
  fi
fi

if [ "$failed" != 0 ]; then
  die "$failed file(s) failed; see above"
fi
echo "All pinned files match deploy/server.json."
