---
name: ship-pr
description: Ship a change in this repo from branch to pull request - branch from main, run the same checks CI runs, write commits and the PR body in the repo's style, pick the labels that decide the release kind, and watch CI. Use whenever you're about to commit, push or open a PR here.
---

# Branch to PR

If `AGENTS.md` exists at the repo root, read it first; where it and this
file disagree, `AGENTS.md` wins.

## Hard rules

- **Branch from an up-to-date `main`. Never commit to `main`, never push to
  `main`, never force-push `main`.** Pushes to `main` publish releases that
  the Pi installs within minutes.
- **No attribution of any kind**, in commits, PR titles, PR bodies, issues
  or comments: no `Co-Authored-By:` trailers, no "Generated with ..." lines,
  no mention of the tool or assistant that helped. The work appears as the
  repo owner's alone.
- Never merge a PR unless the user asks.
- Never commit `docs/` (private notes; it's git-ignored, keep it that way),
  `server.properties`, `connection.private.md`, keys, IP addresses,
  hostnames that aren't already public, or real player names, UUIDs or
  stats. Tests use synthetic fixtures (`web/internal/world/worldtest`,
  `web/internal/nbt/nbttest`).

## 1. Branch

```bash
git switch main && git pull --ff-only
git switch -c <short-topic>        # e.g. play-a11y, mc-agent-reliability
```

## 2. Make the change, with tests

Run what CI runs for the parts you touched (`.github/workflows/ci.yml`,
scopes from `scripts/ci-changes.sh`):

```bash
(cd web && gofmt -l . && go vet ./... && go test -race ./...)   # gofmt -l must print nothing
(cd web && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...) # the Pi is arm64
node --check sites/play/app.js                # sites/play changed
node --check sites/admin/app.js               # sites/admin changed
shellcheck scripts/*.sh                        # scripts/ changed
shellcheck start.sh                            # start.sh changed
scripts/ci-changes_test.sh                     # ci-changes.sh changed
scripts/release-notes_test.sh                  # release-notes.sh changed
actionlint                                     # .github/ changed (go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12)
```

For changes to how play looks, use the `play-preview` skill.

## 3. Commit

Focused commits, each one building and passing tests. Message style, from
`git log`:

```
<area>: <lowercase summary in the imperative>

Optional body: why, and anything a reviewer can't see in the diff.
```

Areas in use: `play`, `admin`, `site`, `mc-agent`, `ci`, `world`, `nbt`,
`ping`, `build-release`, `docs`, `repo` (a package or command name works). Examples:
`site: serve static files with Cache-Control: no-cache`,
`mc-agent: remove units a release no longer lists`.

Before every commit:

```bash
git status --short                 # nothing from docs/, no snapshots, no binaries
git diff --cached                  # read it: no secrets, IPs or player data
```

No trailers. Check with `git log -1 --format=%B` after committing.

## 4. Push and open the PR

```bash
git push -u origin HEAD
gh pr create --base main --title "<plain sentence, capitalized>" \
  --label <kind> --label <area> --body-file <file>
```

Title: a plain sentence such as "Stop Cloudflare from serving last
release's static files". Squash merges reuse it, so keep it specific.

**Labels decide the release kind** (`scripts/release-notes.sh`): a PR
labeled `enhancement` is a feature, else `bug` is a fix, else other; a
release takes the highest kind among its PRs. Always add exactly one of
`enhancement` or `bug` unless the change really is neither. Add area labels
too: `play`, `admin`, `ci-cd`, `infra`, `mc-server`, `security`,
`accessibility`, `tech-debt`. Only changes under `web/`, `sites/`,
`deploy/`, `scripts/build-release.sh` or `.github/workflows/` publish a
release at all.

Body format (see #91 and #93):

```markdown
Fixes #92. One or two sentences of context: what was wrong or missing, and why it matters.

## What's in it
- **Component:** what changed.
  - Detail or consequence worth knowing.
- Another change.

## Testing
- New or changed tests, and what they cover.
- `gofmt`, `go vet` and `go test -race ./...` pass (plus anything else you ran).

## After merging
Only when something must be done by hand (purge a cache, enable a unit). Omit otherwise.
```

Use `Fixes #N` for each issue the PR closes, and `Part of #N` for tracking
issues it only contributes to. For stacked PRs, say "Builds on #N (merge
that first)." at the top.

## 5. Watch CI

```bash
gh pr checks --watch
```

`ci-ok` is the one required check. It passes when every scoped job either
passed or was skipped because nothing in its scope changed. Report `ci-ok`
and the scoped jobs that ran (`go (vet, test, build)`,
`play.ryancaudill.dev`, `admin.ryancaudill.dev and static sites`,
`Minecraft server config`, `deploy files, scripts and workflows`). If a job
fails, read it with `gh run view --log-failed`, fix it in a new commit,
and push.

## 6. After review

- Address review comments with new commits; don't rewrite pushed history
  unless the user asks.
- A stacked PR needs a rebase onto `main` after its base merges:
  `git fetch origin && git rebase origin/main && git push --force-with-lease`
  (only ever on your own branch).
