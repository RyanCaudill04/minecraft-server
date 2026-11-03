---
name: pi-ops
description: Day-to-day operations on the Raspberry Pi over `ssh mc-pi` - service status, logs, the active release vs the newest GitHub Release, deploy, rollback, safe Minecraft restarts, and public site checks. Read-only by default; anything that deploys, rolls back, restarts or stops needs the user's explicit yes first. Use when asked whether the Pi or a site is up, what's deployed, why a deploy didn't happen, to read logs, or to deploy, roll back or restart.
---

# Pi operations

The Pi is reached with `ssh mc-pi`. Everything runs as the login user, with
systemd **user** units. Layout on the Pi:

- `~/minecraft_server`: the server folder (LazyMC, Paper, `world/`, `logs/latest.log`)
- `~/apps/releases/<tag>`: installed releases (tags `r1`, `r2`, ...)
- `~/apps/current`: symlink to the active release; every unit runs from it
- `~/apps/failed`: the tag of the last release that failed its deploy,
  until the next successful switch (deploy or manual rollback) removes it
- `~/apps/active.json`: the active tag and when it was applied

Units (from `deploy/systemd/` and `deploy/release.json`):

| Unit | What | Listens on |
|---|---|---|
| `minecraft` | LazyMC, which starts Paper on demand | 25565 public, Paper on 127.0.0.1:25566 |
| `play` | play.ryancaudill.dev | 127.0.0.1:8082 |
| `admin` | admin.ryancaudill.dev (behind Cloudflare Access) | 127.0.0.1:8081 |
| `site@portfolio` | ryancaudill.dev | 127.0.0.1:8080 |
| `mc-agent.timer` | runs `mc-agent.service` (`mc-agent deploy`) every 2 minutes | |

There is no `site@admin` unit any more; admin is its own `admin` unit.

## Rules

- **Read-only by default.** Only run the commands under "Status", "Logs"
  and "Public checks" without asking.
- **Ask first, every time,** before anything under "Deploy now",
  "Rollback" or "Minecraft server": say exactly what you'll run and what it
  affects, and wait for an explicit yes. A yes covers that one action only.
- **Never edit files on the Pi** (including `~/apps/failed`, units or
  configs). Changes go through a PR; see the `ship-pr` skill.
- Never print or paste secrets, IP addresses or player data from the Pi
  into a PR, issue or commit.
- When the Mac control script (#20) exists, prefer its actions over these
  raw commands.

## Status

```bash
ssh mc-pi 'systemctl --user is-active minecraft play admin site@portfolio mc-agent.timer'
ssh mc-pi '~/apps/current/bin/mc-agent status'          # agent version and active release
ssh mc-pi 'cat ~/apps/active.json; echo; cat ~/apps/failed 2>/dev/null'
ssh mc-pi 'ls -t ~/apps/releases'                        # installed, newest first
ssh mc-pi 'systemctl --user list-timers mc-agent.timer --no-pager'
ssh mc-pi 'curl -fsS http://127.0.0.1:8082/api/status'   # state, online, players
gh release list --limit 3                                # run locally: newest release
```

The health URLs mc-agent checks after a deploy are in `deploy/release.json`.

Why the newest release isn't active yet:

- The timer hasn't fired (up to about 2 minutes after the release).
- Its tag is in `~/apps/failed`: the deploy failed and was rolled back, and
  mc-agent won't retry that tag. Fix it in a PR; the next release deploys.
  The marker only lasts until the next successful switch, so a manual
  rollback clears it and the timer will try the failed tag again.
- The CI/CD run didn't publish one: check `gh run list --branch main --limit 5`.
  Only changes under `web/`, `sites/`, `deploy/`, `scripts/build-release.sh`
  or `.github/workflows/` publish a release.

## Logs

Use `--user-unit`. `journalctl --user -u <unit>` finds nothing on this Pi.

```bash
ssh mc-pi 'journalctl --user-unit mc-agent.service -n 50 --no-pager'   # deploys
ssh mc-pi 'journalctl --user-unit play -n 100 --no-pager'
ssh mc-pi 'journalctl --user-unit minecraft -n 100 --no-pager'         # LazyMC
ssh mc-pi 'tail -n 100 ~/minecraft_server/logs/latest.log'             # Paper
```

Add `--since "1 hour ago"` to narrow it down. Don't paste player names or
IPs from these logs into anything public.

## Deploy now (ask first)

Runs the same deploy as the timer, instead of waiting for it:

```bash
ssh mc-pi 'systemctl --user start mc-agent.service'
ssh mc-pi 'journalctl --user-unit mc-agent.service -n 50 --no-pager'
ssh mc-pi '~/apps/current/bin/mc-agent status'
```

It does nothing if the newest release is already active or is the tag in
`~/apps/failed`. If a deploy fails, mc-agent rolls back to the release that
was active before and writes `~/apps/failed`.

## Rollback (ask first)

`mc-agent rollback` takes no tag. It activates the newest installed
release in `~/apps/releases` (by folder time) that isn't the current one.
After an automatic rollback that is usually the release that just failed,
not an older good one. So work out the target before asking:

```bash
ssh mc-pi 'readlink ~/apps/current; ls -t ~/apps/releases; cat ~/apps/failed 2>/dev/null'
```

The target is the first tag in the `ls -t` list that isn't the current one.

- **If the target is the tag in `~/apps/failed`, don't run it.** It would
  restart every service onto the release that already failed. Tell the
  user, and suggest fixing forward in a PR instead. There is no command to
  roll back to a chosen tag yet (#41).
- Otherwise, name the target tag when you ask, and say that a successful
  rollback **removes `~/apps/failed`**, so the timer will retry the newest
  release, including one that failed.

`rollback` re-enables and starts `mc-agent.timer` (every switch enables the
release's timers), so the timer deploys the newest release again within
about 2 minutes. To keep the rollback while a fix goes through a PR, stop
the timer both before and after, and check nothing is mid-deploy. Name all
four commands in the one ask:

```bash
ssh mc-pi 'systemctl --user stop mc-agent.timer; systemctl --user is-active mc-agent.service'  # expect inactive
ssh mc-pi '~/apps/current/bin/mc-agent rollback'
ssh mc-pi 'systemctl --user stop mc-agent.timer'      # the rollback started it again
ssh mc-pi '~/apps/current/bin/mc-agent status'
```

`stop` leaves the timer enabled, so a reboot brings it back. Tell the user
that, and start it again once the fix is released:

```bash
ssh mc-pi 'systemctl --user start mc-agent.timer'
```

This describes `web/internal/deploy` on `main` today. Once #41 lands,
follow whatever it adds instead.

## Minecraft server (ask first, only when nobody is online)

1. Check `/api/status` (see Status): `online` must be `0`. If it isn't,
   stop and tell the user who is on.
2. Ask the user, naming the action.
3. Stop and confirm Paper saved. `minecraft.service` sends SIGTERM to
   LazyMC and Paper; Paper saves the world and exits (systemd waits up to
   240s):

   ```bash
   ssh mc-pi 'systemctl --user stop minecraft'
   ssh mc-pi 'grep -n "All dimensions are saved" ~/minecraft_server/logs/latest.log | tail -n 1'
   ```

   If Paper was asleep (state `sleeping`), there is nothing to save and no
   new line is expected. If it was `active` and the line is missing, don't
   start it again; show the log tail to the user.
4. Start it again: `ssh mc-pi 'systemctl --user start minecraft'`, then
   check `is-active minecraft` and `/api/status` (expect `sleeping`).

Never run `start.sh` by hand while LazyMC runs; LazyMC starts it itself.

## Public checks

Read-only, from the Mac:

```bash
for url in https://play.ryancaudill.dev/ https://ryancaudill.dev/ https://admin.ryancaudill.dev/; do
  printf '%s %s\n' "$(curl -s -o /dev/null -w '%{http_code}' "$url")" "$url"
done
```

Expect `200` for play and the portfolio, and `302` (to the Cloudflare
Access login) for admin.

Stale Cloudflare caches (#5): compare what play serves with the active
release's files. Release tags are git tags on the commit they were built
from:

```bash
tag=$(ssh mc-pi 'readlink ~/apps/current' | xargs basename)
git fetch --tags origin
for f in app.js style.css; do
  a=$(curl -fsS "https://play.ryancaudill.dev/$f" | shasum -a 256 | cut -d' ' -f1)
  b=$(git show "$tag:sites/play/$f" | shasum -a 256 | cut -d' ' -f1)
  [ "$a" = "$b" ] && echo "$f matches $tag" || echo "$f STALE (not $tag)"
done
```

Do the same for `/shared/base.css` against `sites/shared/base.css`. A
mismatch means a cache is serving an old file; purging Cloudflare is the
user's call, so report it rather than doing it.
