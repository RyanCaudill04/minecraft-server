#!/usr/bin/env bash
# Writes the synthetic worldtest server (made-up players, no real data) into
# <dest>, keeping the modification times play reads "last seen" from.
#
# worldtest.Build only runs inside a Go test, so this drops a throwaway test
# next to it, runs it, and always removes it again.
#
# Usage: .claude/skills/play-preview/fixture.sh <dest>
set -euo pipefail

dest=${1:?usage: fixture.sh <dest>}
root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
mkdir -p "$dest"
dest=$(cd "$dest" && pwd)
helper="$root/web/internal/world/worldtest/zz_preview_fixture_test.go"
trap 'rm -f "$helper"' EXIT

cat > "$helper" <<'EOF'
package worldtest_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/ryancaudill04/minecraft-server/web/internal/world/worldtest"
)

func TestPreviewFixture(t *testing.T) {
	dst := os.Getenv("PLAY_PREVIEW_DEST")
	if dst == "" {
		t.Skip("PLAY_PREVIEW_DEST not set")
	}
	if out, err := exec.Command("cp", "-Rp", worldtest.Build(t)+"/.", dst).CombinedOutput(); err != nil {
		t.Fatalf("copy fixture: %v: %s", err, out)
	}
}
EOF

(cd "$root/web" && PLAY_PREVIEW_DEST="$dest" go test -count=1 -run '^TestPreviewFixture$' ./internal/world/worldtest/ >/dev/null)
echo "synthetic server in $dest"
