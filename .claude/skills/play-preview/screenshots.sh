#!/usr/bin/env bash
# Takes headless Chrome screenshots of a running page: desktop and phone
# width, light and dark. Prints the PNG paths.
#
# Headless Chrome on macOS won't make a window narrower than about 500px,
# so the phone shots load the page in a 390px iframe instead. Motion is
# reduced and virtual time is generous, so entrance animations don't freeze
# mid-fade.
#
# Usage: .claude/skills/play-preview/screenshots.sh <url> <out folder>
set -euo pipefail

url=${1:?usage: screenshots.sh <url> <out folder>}
out=${2:?usage: screenshots.sh <url> <out folder>}
chrome=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
mkdir -p "$out"
out=$(cd "$out" && pwd)

# A separate profile, so this never touches (or waits on) your own Chrome.
profile=$(mktemp -d)
trap 'rm -rf "$profile"' EXIT

cat > "$out/phone.html" <<EOF
<!doctype html>
<meta charset="utf-8">
<style>html,body{margin:0;background:#888}iframe{display:block;width:390px;height:844px;border:0}</style>
<iframe src="$url"></iframe>
EOF

shoot() { # name, width, height, scheme (1 light, 0 dark), target
  "$chrome" --headless=new --disable-gpu --hide-scrollbars --no-first-run \
    --user-data-dir="$profile" --force-prefers-reduced-motion \
    --blink-settings=preferredColorScheme="$4" \
    --virtual-time-budget=8000 \
    --window-size="$2,$3" --screenshot="$out/$1.png" "$5" 2>/dev/null
  echo "$out/$1.png"
}

shoot desktop-light 1440 1000 1 "$url"
shoot desktop-dark 1440 1000 0 "$url"
shoot phone-light 500 844 1 "file://$out/phone.html"
shoot phone-dark 500 844 0 "file://$out/phone.html"
