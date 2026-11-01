---
name: play-preview
description: Preview the play dashboard (sites/play, web/cmd/play) locally before a PR. Builds play, serves it against a world snapshot or the synthetic worldtest fixture, and takes headless Chrome screenshots at desktop and phone width in light and dark mode. Use after changing sites/play, sites/shared or web/cmd/play, or when asked to preview, screenshot or check how play looks.
---

# Preview play locally

Run everything from the repo root. The helper scripts live next to this file.

## 1. Pick a free port and a scratch folder

Use one port for play and two unused ones for Paper and LazyMC. Check that
nothing is listening first. Leftover servers have collided with ports before:

```bash
port=19437 paper=19438 lazymc=19439
lsof -nP -iTCP:$port -iTCP:$paper -iTCP:$lazymc -sTCP:LISTEN   # must print nothing
work=$(mktemp -d -t play-preview)   # outside the repo
```

## 2. Get world data

play reads `<server>/usercache.json` and `<server>/world/...`, so the
snapshot folder must use the same layout as `~/minecraft_server` on the Pi.

**Synthetic data (default, nothing private):**

```bash
.claude/skills/play-preview/fixture.sh "$work/server"
```

This runs `worldtest.Build` (players Alice, Bob and Carol, all made up)
through a throwaway Go test that the script always deletes again. Check
`git status` afterwards: `web/internal/world/worldtest/zz_preview_fixture_test.go`
must not be there.

**Real data (only when the change needs it):**

```bash
.claude/skills/play-preview/snapshot.sh "$work/server"
```

This runs `tar` over `ssh mc-pi` (read-only) and copies `level.dat`,
`world_clocks.dat`, `weather.dat`, `players/{stats,data,advancements}` and
`usercache.json`, keeping modification times ("last seen" comes from them).
The snapshot holds real player data: never copy it into the repo, never
commit it, never paste player names or UUIDs into a PR or issue, and use
synthetic data for any screenshot that goes into a PR.

## 3. Live status (optional)

By default, point `-paper` and `-lazymc` at the unused ports and play shows
the server as offline. For live status, ask the user first, then tunnel to
Paper (25566) and LazyMC (25565) on the Pi. Status pings never wake Paper:

```bash
ssh -N -L $paper:127.0.0.1:25566 -L $lazymc:127.0.0.1:25565 mc-pi &
tunnel=$!
```

## 4. Build and run play

```bash
(cd web && go build -o "$work/play" ./cmd/play)
"$work/play" -dir sites/play -shared sites/shared -server "$work/server" \
  -addr 127.0.0.1:$port -paper 127.0.0.1:$paper -lazymc 127.0.0.1:$lazymc \
  > "$work/play.log" 2>&1 &
server=$!
for _ in $(seq 50); do curl -fs http://127.0.0.1:$port/ >/dev/null && break; sleep 0.1; done
curl -fsS http://127.0.0.1:$port/api/status
```

`-shared` is required too: the page loads `/shared/base.css`.

## 5. Screenshots

```bash
.claude/skills/play-preview/screenshots.sh http://127.0.0.1:$port/ "$work/shots"
```

This writes `desktop-light.png`, `desktop-dark.png`, `phone-light.png` and
`phone-dark.png`. It uses `--blink-settings=preferredColorScheme=1` (light)
and `=0` (dark), reduced motion, and an 8 second `--virtual-time-budget` so
entrance animations finish. Headless Chrome on macOS can't make a window
narrower than about 500px, so the phone shots load the page in a 390px
iframe (the grey strip on the right is outside the phone). Set `CHROME=` to
use a different Chrome binary.

Open each PNG and look at it: layout at both widths, both themes, text
contrast, nothing clipped or stuck mid-fade. Show the user the four paths.

## 6. Clean up (always, even if a step failed)

```bash
kill $server ${tunnel:-} 2>/dev/null
lsof -nP -iTCP:$port -sTCP:LISTEN   # must print nothing
rm -rf "$work"                      # deletes any real snapshot too; copy out
                                    # screenshots you still need first
git status --short                  # no snapshot, fixture test or PNGs
```
