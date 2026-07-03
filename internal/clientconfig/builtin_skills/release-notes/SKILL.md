---
name: release-notes
description: Draft release notes or a changelog entry from the git history between two refs — gather commits/PRs with the exact git commands given, group by breaking changes/features/fixes, rewrite commit-speak into user-facing language, put breaking changes and migration steps first, and link PR numbers. Use it when asked to write release notes, a changelog entry, or a "what changed since <ref>" summary.
---

# Release notes

Turn `git log` between two refs into notes a *user* of the software can act
on. The audience did not read the diffs; write for them.

## Step 1 — gather the raw input

Identify the range: usually `<last-release-tag>..<head>` (ask or infer from
`git tag --sort=-creatordate | head`). Then collect:

```bash
git log --oneline --no-merges <from>..<to>            # every commit subject
git log --merges --format='%s%n%b' <from>..<to>       # merge/PR titles + bodies
git diff --stat <from>...<to>                          # blast radius sanity check
git log --format='%s' <from>..<to> | grep -iE 'break|migrat|deprecat|remov' # breaking-change candidates
```

If the repo squash-merges, the `--no-merges` log is the PR list itself; PR
numbers appear as `(#123)` suffixes on subjects. Keep those numbers.

## Step 2 — classify every commit

Bucket each item as one of:

- **Breaking** — removed/renamed APIs, changed defaults, config or schema
  migrations, dropped platform support. When in doubt, read the commit body or
  diff; do not guess.
- **Feature** — new user-visible capability.
- **Fix** — corrected behavior a user could have hit.
- **Internal** — refactors, CI, tests, docs plumbing. These are *omitted* from
  the notes unless they affect users (e.g. performance, build requirements).

Collapse multi-commit work into one entry per user-facing change — the unit is
the change, not the commit.

## Step 3 — rewrite into user-facing language

For each entry, state what the user can now do or no longer suffers, not what
the code does:

- Commit-speak: `fix nil deref in sched retry path (#412)`
- Release note: `Scheduled tasks no longer crash when a retry fires after the
  task was deleted. (#412)`

Rules: start with the user-visible effect; present tense; no file paths,
function names, or internal package names unless they are the public API; keep
the `(#PR)` link on every entry.

## Step 4 — assemble, breaking changes first

```markdown
## <version> — <YYYY-MM-DD>

### Breaking changes
- <what broke, who is affected, and the exact migration step — command,
  config rename, or API replacement>. (#NNN)

### Features
- ... (#NNN)

### Fixes
- ... (#NNN)
```

- Breaking changes come **first**, each with concrete migration instructions —
  "update your config" is not an instruction; `rename sandbox.image to
  sandbox.base_image` is.
- Order entries within a section by user impact, not chronology.
- If a section is empty, omit it.

## Step 5 — verify before delivering

- Cross-check: every breaking-candidate from Step 1's grep is either listed
  under Breaking changes or consciously ruled out.
- Every entry has a PR/commit reference; every claim is traceable to one.
- Read the notes once as the user: could you upgrade using only this text? If
  a migration step is unclear, go back to the diff and make it exact.
