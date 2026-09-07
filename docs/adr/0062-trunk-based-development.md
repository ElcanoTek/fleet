# ADR-0062: Trunk-based development — `main` is the only branch

- **Status:** Accepted
- **Date:** 2026-09-07
- **Deciders:** fleet maintainers
- **Amends:** ADR-0059 (the `dev` → `main` promotion it describes no longer
  exists; the tagging is unchanged), ADR-0061 (the "promotion body" is now
  simply the PR's squash commit message)

## Context

fleet ran two long-lived branches. `dev` integrated feature PRs behind a fast
CI lane (`dev-ci.yml`); a **promotion PR** squash-merged `dev` into `main`
behind the full gate (`ci.yml`); `release.yml` tagged every green push to
`main`. The shape was chosen so that expensive CI (the `-race` lane,
govulncheck, the Grype image scan, both Playwright suites) ran once per batch
rather than once per PR.

It cost more than it saved, and the costs were structural rather than
incidental:

- **Squash-merging between two long-lived branches breaks shared history.**
  The squash puts a commit on `main` that `dev` never has, so the merge-base
  never advances and every region `dev` changes twice reads as
  both-sides-modified on the next promotion — six files of phantom conflicts
  on #1293 with `main` not having moved at all. The repair was a workflow
  (`promotion-ancestry.yml`, #1298) that recorded a `-s ours` merge back onto
  `dev` after every promotion, gated on tree identity, with a documented
  manual procedure for when the gate refused. That is a mechanism whose only
  job is to undo a wound the branching model inflicts on itself; GitHub's own
  guidance names squash-between-long-lived-branches as the case to avoid.
- **Two CI lanes with different job sets meant the promotion was the first
  time the full gate saw the code.** A change could pass `dev`, sit for a day,
  and fail `-race` or the live e2e at promotion, where it was mixed with
  everything else that had landed and far more expensive to unpick. `Dev
  gate` was also never a required check on `dev`, so the fast lane was
  red-but-not-required — a red X beside a mergeable PR (docs/SCANNING.md,
  "Known gaps", now closed).
- **Release notes had no natural source.** GitHub's generated notes see the
  PRs merged into `main` since the previous tag, and with squash promotions
  that is exactly one PR — the promotion — never the `dev` PRs behind it. The
  promotion PR body had to be hand-written as the release notes, then carried
  into the release by workflow (ADR-0061), and an afternoon of edge cases in
  how to carry it was the immediate trigger for this ADR.
- **Every agent and contributor had to learn a ritual**: which branch to
  target, merge-commit on `dev` but squash on `main`, how to write a
  promotion, when to run the ancestry merge by hand. None of it was about
  fleet.

The repositories fleet takes its agent-development posture from —
openai/codex, anthropics/claude-code, basecamp/omarchy — all run trunk-based:
one `main`, every PR squash-merged into it, releases cut from tags on `main`.

## Decision

**`main` is the only long-lived branch.** Every PR targets `main` and is
**squash-merged**, so one PR is one commit on `main`. `dev` is retired (it was
deleted on 2026-09-07, when the last promotion, #1451, merged with head-branch
auto-deletion on) and is not recreated.

**One CI workflow.** `ci.yml` (`CI`) runs the full gate on every PR into `main`
and every push to `main`; `CI gate` is the single required status check.
`dev-ci.yml` and `promotion-ancestry.yml` are deleted. The docs-only
classifier still lets a prose-only PR skip the heavy jobs, and the gate accepts
exactly that skip. The full gate on every PR costs about 25 minutes per PR,
most of it the `-race` lane; that is the price of "a green PR is a shippable
change", and it replaces a fast lane whose green meant less than it looked.

**Releases are unchanged in mechanism** (ADR-0059): every green push to `main`
is tagged `vYYYY.MM.DD.N` by `release.yml`, several a day is normal, and
nobody types a version. What changes is the input: the squash commit message
is the PR's title and body (GitHub prefills the merge dialog from them), and
that message is the release notes, ahead of GitHub's generated list (ADR-0061,
as amended by #1452). Only the newest release object is kept on the Releases
page; tags are permanent (#1453).

**Dependabot targets `main`** (the default), so version bumps go through the
same gate as everything else instead of soaking on an integration branch.

## Consequences

- No promotions, no promotion PR body, no ancestry merge, no manual
  `-s ours` procedure. CONTRIBUTING.md loses its "Promotions" section; the
  steward skill and `docs/PR-STEWARDSHIP.md` lose their promotion rules.
- The PR is the first and last time CI sees a change. Whoever drives the PR
  drives it through the whole gate; there is no later, more expensive place
  for a failure to surface.
- The red-but-not-required gap is closed by construction: there is one
  branch, and its gate is required.
- `fleet update` boxes track `main`, and a red push to `main` gets no tag
  (ADR-0059). With the full gate pre-merge, a red `main` can only come from
  a post-merge environmental failure (a new CVE in an unchanged dependency, a
  registry outage) — the same cases the scheduled scans already exist for.
  The fix is the next PR.
- Several docs that described the two-lane shape were rewritten in the same
  PR; historical passages (dated incidents on `dev`, run numbers) are kept as
  history and marked as such.
- Operator actions outside the repository, done by hand once: delete the
  `dev` ruleset; keep `main`'s ruleset requiring `CI gate` but **without**
  "require branches to be up to date" (every merge would otherwise send every
  other open PR back through the 25-minute gate, for a re-run of a check that
  already passed on its head; the post-merge gate on `main` refuses to tag a
  red push, which is the protection that matters); repository merge settings
  allow squash only, with the default squash message set to "Pull request
  title and description"; head branches auto-delete on merge.

## Alternatives considered

- **Keep `dev`, switch promotions to merge commits.** Fixes the phantom
  conflicts (the merge-base advances) and makes GitHub's generated notes see
  the `dev` PRs. Keeps everything else: two lanes, the promotion ritual, the
  late first sight of the full gate. Rejected as half the fix.
- **Trunk-based, but a fast lane on PRs and the heavy jobs only on push to
  `main`.** Faster PRs, and `release.yml` already refuses to tag a red push.
  Rejected because boxes track `main`, not tags: a red push to `main` is code
  a `fleet update` will build and run. The gate belongs before the merge.
- **GitHub merge queue** to run the full gate on the merge group rather than
  on each PR head. The right next step if PR-time CI becomes the bottleneck;
  not needed to retire `dev`, and it adds a ruleset moving part. Deferred.
