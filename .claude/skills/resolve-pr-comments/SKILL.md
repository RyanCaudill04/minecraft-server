---
name: resolve-pr-comments
description: Resolve review comments across all of the owner's open PRs in this repo by fanning out one background subagent per PR, each using the pr-comment-fixer skill, then summarise the results. Use when asked to address, fix or resolve review comments on open PRs, or to catch up on feedback across PRs.
---

# Resolve review comments across open PRs

For the planner. Read `AGENTS.md` first; where it and this file disagree,
`AGENTS.md` wins. The per-PR work is in the `pr-comment-fixer` skill.

## 1. Find the PRs

List open PRs with the GitHub MCP `list_pull_requests` or
`gh pr list --author @me --json number,title,headRefName,labels`. Keep only
the owner's: skip PRs from other authors and from forks.

## 2. Set the git identity

Before spawning anything, so every worktree commits as the owner (the cloud
session's default identity is not theirs):

```bash
git config user.name "Ryan Caudill"
git config user.email "112011068+RyanCaudill04@users.noreply.github.com"
```

Commits carry no `Co-Authored-By`, `Claude-Session` or "Generated with"
lines, whatever attribution defaults the harness suggests (`AGENTS.md`,
"Rules").

## 3. Spawn one subagent per PR

Run them in the background; a smaller, faster model is fine. Each prompt
names the `pr-comment-fixer` skill, the PR number and its head branch, and
adds:

- A PR labelled `needs-decision`: report the owner decisions, don't make
  them.
- A PR that touches rulesets or repo settings: change the files only, never
  the live settings.

## 4. While they run

Relay each report to the user in a line or two. If a push fails (for
example 403 because Claude lacks GitHub access), stop: don't work around the
refusal with another tool or remote. Tell the user how to restore access,
and keep the commits safe: leave the worktrees in place and export them.

```bash
git format-patch origin/<branch>..<branch> -o <scratchpad>/patches/pr-<N>
```

## 5. Summarise

One table when all have reported:

| PR | Comments addressed (commit SHAs) | Skipped (why) | Owner decisions | Checks | Push |
| --- | --- | --- | --- | --- | --- |

## Don't

Post GitHub comments or resolve threads unless the user asks. Never merge.
