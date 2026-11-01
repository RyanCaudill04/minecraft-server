---
name: repo-review
description: Review a diff, branch or PR in this repo against its own checklist, built from bugs past reviews found - deploy agent, CI workflow and scripts, the nbt/world/ping parsers, the play frontend, and repo rules (no attribution, no private data). Reports only verified findings with file:line and a failure scenario. Use when asked to review a change or PR here, and as the checklist for the CI review step (#58).
---

# Repo review

## Scope

- A PR: `gh pr diff <n>` and `gh pr view <n>` (read the body: does the
  change do what it says?).
- A branch: `git diff origin/main...HEAD`.
- Local work: `git diff` and `git diff --cached`.

Read the surrounding code, not just the hunks: most past bugs were in how a
change interacts with code it didn't touch. Only review the areas the diff
touches, using the matching sections below, plus "Repo rules" always.

## Checklist

### Deploy agent (`web/internal/deploy`, `web/cmd/mc-agent`)

- **Interrupted deploys (#40):** if the process dies between switching
  `~/apps/current` and the health check, is the next run able to tell and
  finish or undo it? An unverified release must not be treated as active
  and done.
- **Rollback and the `failed` marker (#41):** a manual or automatic
  rollback must not be undone by the next timer run, and must never roll
  forward to the tag in `<apps>/failed`.
- **Units added or removed between releases (#42):** units a release no
  longer lists get stopped, disabled and unlinked, including after a
  rollback and for template instances (`site@<name>.service`).
  `<apps>/managed.json` must stay in step.
- **An agent that can't run after a switch (#45):** the new release's
  `bin/mc-agent` must be checked, because the timer runs
  `%h/apps/current/bin/mc-agent`. Health checks of the web apps alone won't
  catch a broken agent.
- Every switch of `current` writes `active.json`; failures after the link
  has switched are logged, not returned as if nothing happened.

### CI and scripts (`.github/workflows/ci.yml`, `scripts/`)

- **Release decision vs cancelled or failed runs (#46):** main's runs diff
  against the newest `r<N>` tag, not the previous push, so nothing
  deployable is skipped. Watch `if:` conditions: without a status function
  GitHub adds `success()`, which is false when a needed job was skipped.
- **Path quoting (#47):** `git` quotes non-ASCII paths unless
  `core.quotePath=false`; `scripts/ci-changes.sh` must fail on quoted
  input, not silently match nothing.
- **No `${{ }}` inside `run:`** for anything that isn't a fixed literal:
  pass it through `env:` and quote `"$VAR"`.
- **Pinned actions:** every `uses:` pinned to a full commit SHA with a
  `# vX.Y.Z` comment.
- **Least privilege:** workflow `permissions: contents: read`; only the
  job that needs more (release) widens it, with a comment saying why.
  `persist-credentials: false` on checkouts.
- Shell: `set -euo pipefail` in scripts, `set -o pipefail` before `| tee`,
  quoted variables, shellcheck-clean. New scope patterns in
  `ci-changes.sh` need a case in `ci-changes_test.sh`.
- `deploy/release.json` and `deploy/systemd/` agree (the infra job checks
  this; make sure a new unit is covered).

### Parsers (`web/internal/nbt`, `web/internal/world`, `web/internal/ping`)

- **Allocations sized by untrusted lengths (#35):** never `make` from a
  length read off disk or the network before checking it against the bytes
  left or a hard limit.
- **One bad file failing everything (#38):** a half-written or corrupt
  stats, advancements, player data or `usercache.json` file must cost only
  that player or field, not the whole response. The server writes these
  files while play reads them.
- **Tests that really exercise limits (#39):** a limit test must hit the
  limit (one over and exactly at it), not pass because of an earlier,
  unrelated error.
- Errors wrapped once (no doubled prefixes); timeouts surface as
  `context.DeadlineExceeded`.

### Frontend (`sites/play`, `sites/admin`, `sites/shared`)

- **Focus loss on re-render (#49):** polling re-renders must not replace
  the element that has focus.
- **Modal behavior (#50):** a dialog traps focus, restores it on close,
  closes on Escape, and stops the background scrolling.
- **Keyboard access (#51):** everything clickable is reachable and usable
  with the keyboard; ARIA patterns (tabs) are complete or not used.
- **Contrast (#54):** text and controls meet WCAG AA in light *and* dark.
- **Go's zero time (#34):** a missing timestamp arrives as
  `0001-01-01T00:00:00Z`; the page must treat it as "never", not as a date.
- **`textContent` only, never `innerHTML`** (or `insertAdjacentHTML`,
  `outerHTML`) for anything from the API, including player names and PR
  titles.
- Check it visually with the `play-preview` skill when layout changes.

### Repo rules (always)

- No attribution anywhere: no `Co-Authored-By:` trailers, no
  "Generated with ..." lines, no tool or assistant mentions in commits, PR
  title or body (`git log --format=%B origin/main..HEAD`).
- Nothing from `docs/`, no `server.properties`, keys or
  `connection.private.md`.
- No IP addresses, secrets, Access team details, or real player names,
  UUIDs or stats, in code, tests, fixtures, comments or the PR body.
- Tests use synthetic fixtures (`worldtest`, `nbttest`).
- The PR follows the `ship-pr` skill: labels (`enhancement` or `bug`
  decides the release kind), body sections, tests for the change.

## Verify before reporting

For each candidate finding, confirm it is real: read the code path end to
end, and where you can, prove it with a quick test or command (keep any
scratch test out of the commit). Drop anything you can't confirm, style
preferences, and anything CI already enforces (gofmt, vet, shellcheck,
actionlint).

## Output

Only verified findings, most severe first:

```
<file>:<line> - <one-line summary>
Scenario: the concrete input or sequence of events that makes it fail, and what the user or the Pi sees.
Fix: the smallest change that addresses it (optional).
```

If there are none, say so in one line. Don't pad with praise or nits.

## Filing issues (only when asked)

1. Check for duplicates first: `gh issue list --state all --search "<keywords>"`.
   If one exists, comment there instead.
2. `gh issue create` with a plain title, a body with the file:line,
   scenario and suggested fix, and existing labels only (`gh label list`):
   `from-review`, `bug` or `enhancement`, and an area (`play`, `admin`,
   `ci-cd`, `infra`, `mc-server`, `security`, `accessibility`,
   `tech-debt`).
3. No attribution in the issue, same as everywhere else.
