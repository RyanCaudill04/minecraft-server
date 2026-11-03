# Rulesets

The repo's GitHub rulesets, kept here so changes to them are reviewed like
code. GitHub doesn't read these files; apply them with
[`scripts/apply-rulesets.sh`](../../scripts/apply-rulesets.sh) as the repo
owner:

```bash
scripts/apply-rulesets.sh --dry-run   # print the API calls
scripts/apply-rulesets.sh             # make them
```

Each file replaces the repo's ruleset with the same `name`, or creates it.
The script also sets the repo settings from #7 that rulesets can't hold (see
its header).

- [`main.json`](main.json): `main` takes changes only through squash-merged
  pull requests, with linear history, and only once `ci-ok` passes. The
  check must come from GitHub Actions (app id 15368), so a status posted by
  anything else doesn't count. Branches don't need to be up to date first,
  since each run on `main` re-checks everything since the last release.
- [`release-tags.json`](release-tags.json): nobody can create, move or
  delete an `r*` tag except GitHub Actions, so only the release job can
  mint the tags the Pi installs from.

After applying it, check that the next release on `main` still creates its
tag. If the Actions bypass on `release-tags.json` doesn't take effect, the
release job fails at `gh release create`; set that ruleset's enforcement to
`disabled` in Settings → Rules until it's fixed.
