# AGENTS.md

Instructions for coding agents (and people) working in this repo. `CLAUDE.md`
imports this file, so every tool reads the same rules. Where a skill under
`.claude/skills/` disagrees with this file, this file wins.

## Layout

| Path | What it is |
| --- | --- |
| `web/` | One Go module (`web/go.mod`) for every web app and the deploy agent. |
| `web/cmd/play` | play.ryancaudill.dev: server status, world facts and player stats. Listens on `127.0.0.1:8082`. |
| `web/cmd/admin` | admin.ryancaudill.dev: live status and the active release. Listens on `127.0.0.1:8081`. |
| `web/cmd/serve` | Plain static file server for the static sites (ryancaudill.dev is `sites/portfolio`, on `127.0.0.1:8080`). |
| `web/cmd/mc-agent` | The deploy agent on the Pi: `deploy`, `rollback`, `status`, `version`. |
| `web/cmd/mc-players` | Small CLI that prints who is online with a status ping. |
| `web/internal/*` | Shared packages: `deploy` (mc-agent's logic), `nbt` and `world` (read world files), `ping` and `mcstatus` (server status), `site` (static files and JSON helpers). |
| `web/internal/world/worldtest`, `web/internal/nbt/nbttest` | Synthetic test fixtures. Tests never use real player data. |
| `sites/play`, `sites/admin`, `sites/portfolio` | Each site's `index.html`, `style.css`, `app.js`. |
| `sites/shared` | `base.css`, served at `/shared/` by play and admin. |
| `deploy/systemd/` | systemd **user** units installed on the Pi. |
| `deploy/sites/*.env` | Listen address for each `site@<name>` instance. |
| `deploy/release.json` | Units, services, timers and health checks mc-agent applies for a release. |
| `scripts/` | CI change detection, release notes and the release build, each with a `_test.sh` where it has logic. |
| `.github/workflows/ci.yml` | CI/CD: scoped checks on PRs, one required check `ci-ok`, and releases from `main`. |
| `.github/workflows/review.yml`, `.github/review.md` | Optional advisory review on PRs labelled `review` (see below). |
| `.claude/skills/` | Repo-specific agent skills, added in #105 (see "Decision: agent instructions and skills"). |
| Repo root | The Minecraft server's own files: `lazymc.toml`, `start.sh`, `server.properties.example`, Paper/Spigot/Bukkit configs, `plugins/`, `config/`. |

## Build and test

Run what CI runs for the parts you touched. `scripts/ci-changes.sh` decides
the scopes; a change under `.github/` runs every scope.

```bash
# Go (always): gofmt -l must print nothing
(cd web && gofmt -l . && go vet ./... && go test -race ./...)
(cd web && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...)   # the Pi is arm64

# Frontend
node --check sites/play/app.js
node --check sites/admin/app.js

# Scripts, workflows and the release
shellcheck scripts/*.sh start.sh
scripts/ci-changes_test.sh
scripts/release-notes_test.sh
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12   # CI pins this version
scripts/build-release.sh <tag> <output folder> [notes.json]   # e.g. scripts/build-release.sh dev "$(mktemp -d)"
```

### Preview play or admin locally

Nothing listens on the `-paper` and `-lazymc` ports below, so the server
reads as offline; an empty `-server` folder makes `/api/world` return 503.
Open http://127.0.0.1:8082/ (play) or http://127.0.0.1:8081/ (admin).

```bash
(cd web && go run ./cmd/play -dir ../sites/play -shared ../sites/shared \
  -server "$(mktemp -d)" -addr 127.0.0.1:8082 -paper 127.0.0.1:1 -lazymc 127.0.0.1:2)

(cd web && go run ./cmd/admin -dir ../sites/admin -shared ../sites/shared \
  -apps "$(mktemp -d)" -addr 127.0.0.1:8081 -paper 127.0.0.1:1 -lazymc 127.0.0.1:2)
```

For play with synthetic world and player data, and screenshots, use the
`play-preview` skill (#105).

### Tests

New behavior comes with tests, and every bug fix gets a regression test.
Fixtures are synthetic (`worldtest`, `nbttest`).

## Workflow

- Branch from an up-to-date `main` and open a pull request. `main` changes
  only through PRs that pass `ci-ok`. Never commit to, push to or
  force-push `main`.
- Merging to `main` deploys automatically: CI publishes a release `r<N>`
  when anything under `web/`, `sites/`, `deploy/`, `.github/workflows/` or
  `scripts/build-release.sh` changed, and `mc-agent.timer` on the Pi
  installs it within minutes. Nobody edits files on the Pi, and the Pi's
  git clone isn't pulled.
- Commits: `<area>: <lowercase imperative summary>`, for example
  `site: serve static files with Cache-Control: no-cache`. Areas in use
  include `play`, `admin`, `site`, `mc-agent`, `ci`, `world`, `nbt`,
  `ping`, `build-release`, `docs`.
- PR title: a plain, specific sentence (squash merges reuse it). PR body,
  as in #91 and #93: `Fixes #N` (or `Part of #N`) and a sentence of
  context, then `## What's in it`, `## Testing`, and `## After merging`
  only when something must be done by hand.
- Labels decide the release kind (`scripts/release-notes.sh`):
  `enhancement` is a feature, else `bug` is a fix, else other. Add one of
  them unless the change is neither, plus area labels (`play`, `admin`,
  `ci-cd`, `infra`, `mc-server`, `security`, `accessibility`,
  `tech-debt`, `documentation`). Use only labels that exist
  (`gh label list`).
- Never merge a PR unless the owner asks.

## Rules

- **No tool or AI attribution** in commits, PR titles, PR descriptions,
  issues or comments: no `Co-Authored-By` trailers, no "Generated with"
  lines, no mention of the assistant that helped. Work appears as the
  owner's alone. Check with `git log --format=%B origin/main..HEAD`.
- `docs/` is private and git-ignored. Never commit it, quote it, or
  reference its contents from tracked files.
- The repo is public: no IP addresses, secrets, tokens, Access team
  details, connection details or real player data. Test fixtures are
  synthetic.
- `server.properties` stays untracked; edit `server.properties.example`.
- **Don't touch production without explicit confirmation.** Never ssh to
  the Pi, stop or restart the live server or its services, run
  `mc-agent deploy` or `rollback`, force-push, or change repo settings or
  secrets unless the owner has said yes to that specific action.

## Pi facts

- Services are systemd **user** units. Read logs with
  `journalctl --user-unit <name>` (`--user -u` finds nothing on this Pi).
- Web apps run from `~/apps/current`, a link to a folder in
  `~/apps/releases` that mc-agent switches.
- LazyMC owns port 25565 and starts Paper on demand; Paper listens only on
  `127.0.0.1:25566`.

## Advisory review (#58)

`.github/workflows/review.yml` runs a model-based review when a PR gets the
`review` label, using the checklist in `.github/review.md`. It is advisory:
it is not part of `ci-ok`, can't block a merge, and does nothing until the
`CLAUDE_CODE_OAUTH_TOKEN` repo secret exists. Fork PRs never get the
secret. Remove and re-add the label to review again after new pushes.

## Decision: agent instructions and skills

Part of #60.

- **This file is committed** at the repo root, and `CLAUDE.md` is a
  one-line `@AGENTS.md` import, so tools that read either file get the same
  rules. Nothing in it is private.
- **Skills are committed under `.claude/skills/<name>/SKILL.md`**, the
  location Claude Code loads project skills from, as #105 does. They are
  public like everything else here, so the rules above apply to them.
- **How they relate:** `AGENTS.md` holds the rules and facts that apply to
  every task; a skill holds the steps for one recurring workflow (preview
  play, Pi operations, shipping a PR, review). Skills point here instead of
  repeating the rules, and this file wins when they disagree.
- The CI review (#58) reads `.claude/skills/repo-review/SKILL.md` as its
  checklist when it exists, so the local and CI reviews stay the same.
