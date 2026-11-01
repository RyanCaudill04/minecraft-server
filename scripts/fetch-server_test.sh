#!/usr/bin/env bash
# Tests for fetch-server.sh. Each case runs it against a made-up manifest
# whose URLs are local file:// paths, and a stand-in java for --patch, so
# it needs no network and runs the same on a Mac and in CI.
#
# Usage: scripts/fetch-server_test.sh
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/fetch-server_test.XXXXXX")
trap 'rm -rf "$work"' EXIT
failures=0

fail() { echo "FAIL $*"; failures=$((failures + 1)); }
if command -v sha256sum >/dev/null; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
else
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
fi

# Upstream files and a manifest that pins them.
up=$work/upstream
mkdir -p "$up"
echo paperclip >"$up/paper.jar"
echo lazymc-arm >"$up/lazymc-arm64"
echo lazymc-amd >"$up/lazymc-amd64"
echo plugin-a >"$up/a.jar"
echo patched >"$up/patched.jar"

# manifest <file> [jq filter applied to the good manifest]
manifest() {
  jq -n --arg up "$up" \
    --arg paper "$(sha256 "$up/paper.jar")" --arg arm "$(sha256 "$up/lazymc-arm64")" \
    --arg amd "$(sha256 "$up/lazymc-amd64")" --arg a "$(sha256 "$up/a.jar")" \
    --arg patched "$(sha256 "$up/patched.jar")" '{
      minecraft: "1.0", java: 25,
      paper: {build: 1, path: "server.jar", url: "file://\($up)/paper.jar", sha256: $paper,
              patched: {path: "versions/1.0/paper-1.0.jar", sha256: $patched}},
      lazymc: {version: "0.1", path: "lazymc", builds: {
        arm64: {url: "file://\($up)/lazymc-arm64", sha256: $arm},
        amd64: {url: "file://\($up)/lazymc-amd64", sha256: $amd}}},
      plugins: [{name: "A", version: "1", source: "x", path: "plugins/A-1.jar",
                 url: "file://\($up)/a.jar", sha256: $a}]
    }' | jq "${2:-.}" >"$1"
}
good=$work/good.json
manifest "$good"

# A stand-in java: Paperclip's patch-only run writes the patched jar.
bin=$work/bin
mkdir -p "$bin"
cat >"$bin/java" <<EOF
#!/usr/bin/env bash
mkdir -p versions/1.0
cp "\${FAKE_PATCHED:-$up/patched.jar}" versions/1.0/paper-1.0.jar
EOF
chmod +x "$bin/java"

fetch() { PATH="$bin:$PATH" "$dir/fetch-server.sh" --manifest "${mf:-$good}" "$@"; }

# case <name>: a fresh server folder in $srv
case_dir() { srv=$work/$1; mkdir -p "$srv"; }

case_dir fresh
if out=$(fetch --arch arm64 "$srv" 2>&1); then
  cmp -s "$srv/server.jar" "$up/paper.jar" || fail "fresh: server.jar"
  cmp -s "$srv/lazymc" "$up/lazymc-arm64" || fail "fresh: lazymc should be the arm64 build"
  [ -x "$srv/lazymc" ] || fail "fresh: lazymc should be executable"
  cmp -s "$srv/plugins/A-1.jar" "$up/a.jar" || fail "fresh: plugin"
  grep -q 'missing  versions/1.0/paper-1.0.jar' <<<"$out" || fail "fresh: should say the patched jar is missing: $out"
  [ -z "$(find "$srv" -name '*.download.*')" ] || fail "fresh: left a temp file"
else
  fail "fresh: exit $?: $out"
fi

# Re-running downloads nothing: hide upstream to prove it.
mv "$up" "$up.hidden"
if out=$(fetch --arch arm64 "$srv" 2>&1); then
  [ "$(grep -c '^  ok ' <<<"$out")" = 3 ] || fail "rerun: want 3 ok lines: $out"
else
  fail "rerun: exit $?: $out"
fi
mv "$up.hidden" "$up"

case_dir amd64
fetch --arch amd64 "$srv" >/dev/null 2>&1 || fail "amd64: exit $?"
cmp -s "$srv/lazymc" "$up/lazymc-amd64" || fail "amd64: lazymc should be the amd64 build"

case_dir badarch
if fetch --arch riscv64 "$srv" >/dev/null 2>&1; then fail "badarch: want a non-zero exit"; fi

case_dir mismatch
fetch --arch arm64 "$srv" >/dev/null 2>&1
echo changed >"$srv/server.jar"
if out=$(fetch --arch arm64 "$srv" 2>&1); then
  fail "mismatch: want a non-zero exit"
else
  grep -q 'MISMATCH server.jar' <<<"$out" || fail "mismatch: no MISMATCH line: $out"
  [ "$(cat "$srv/server.jar")" = changed ] || fail "mismatch: must not touch the file without --replace"
fi
if out=$(fetch --arch arm64 --replace "$srv" 2>&1); then
  grep -q 'replaced  server.jar' <<<"$out" || fail "replace: no replaced line: $out"
  cmp -s "$srv/server.jar" "$up/paper.jar" || fail "replace: server.jar not replaced"
else
  fail "replace: exit $?: $out"
fi

case_dir badhash
mf="$work/badhash.json"
manifest "$mf" '.plugins[0].sha256 = ("0" * 64)'
if out=$(fetch --arch arm64 "$srv" 2>&1); then
  fail "badhash: want a non-zero exit"
else
  [ ! -e "$srv/plugins/A-1.jar" ] || fail "badhash: installed a file with the wrong SHA-256"
  [ -z "$(find "$srv" -name '*.download.*')" ] || fail "badhash: left a temp file"
  [ -f "$srv/server.jar" ] || fail "badhash: should still fetch the other files"
fi
unset mf

case_dir unsafe
mf="$work/unsafe.json"
manifest "$mf" '.plugins[0].path = "plugins/../../escape.jar"'
if fetch --arch arm64 "$srv" >/dev/null 2>&1; then fail "unsafe path: want a non-zero exit"; fi
[ ! -e "$work/escape.jar" ] || fail "unsafe path: wrote outside the server folder"
unset mf

case_dir http
mf="$work/http.json"
manifest "$mf" '.paper.url = "http://example.com/paper.jar"'
if fetch --arch arm64 "$srv" >/dev/null 2>&1; then fail "http url: want a non-zero exit"; fi
unset mf

case_dir unlisted
mkdir -p "$srv/plugins"
echo old >"$srv/plugins/A-0.jar"
out=$(fetch --arch arm64 "$srv" 2>&1) || fail "unlisted: exit $?: $out"
grep -q 'warning  plugins/A-0.jar' <<<"$out" || fail "unlisted: no warning: $out"
[ -f "$srv/plugins/A-0.jar" ] || fail "unlisted: must not delete it"

case_dir patch
if out=$(fetch --arch arm64 --patch "$srv" 2>&1); then
  grep -q 'patched  versions/1.0/paper-1.0.jar' <<<"$out" || fail "patch: no patched line: $out"
else
  fail "patch: exit $?: $out"
fi
out=$(fetch --arch arm64 --patch "$srv" 2>&1) || fail "patch rerun: exit $?"
grep -q 'ok       versions/1.0/paper-1.0.jar' <<<"$out" || fail "patch rerun: want ok: $out"

case_dir badpatch
echo wrong >"$work/wrong.jar"
if FAKE_PATCHED=$work/wrong.jar fetch --arch arm64 --patch "$srv" >/dev/null 2>&1; then
  fail "bad patch: want a non-zero exit"
fi

if ((failures)); then
  echo "$failures failure(s)"
  exit 1
fi
echo "fetch-server: all cases pass"
