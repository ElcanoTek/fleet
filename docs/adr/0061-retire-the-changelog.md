# ADR-0061: Retire `CHANGELOG.md`; the PR is the record and the release notes are generated

- **Status:** Accepted
- **Date:** 2026-09-07
- **Deciders:** fleet maintainers

## Context

`CHANGELOG.md` was a 9 100-line hand-written running log with one section,
"Recent changes", that every PR was asked to prepend to. ADR-0059 had already
taken the release history away from it: every green push to `main` is tagged
and published as a GitHub release whose notes are **generated** from the
commits and PR titles since the previous tag. What the file still claimed to
add was the *why* — the prose a generated diff cannot say.

In practice it cost more than it gave:

- **It was the repository's single hottest merge conflict.** Several agents
  and humans land PRs to `dev` a day, and every one of them edited the same
  few lines at the top of the same file. A PR that touched nothing another PR
  touched still conflicted on the changelog, and the resolution was always
  mechanical — keep both — which is the definition of a file that should not
  be shared state.
- **The *why* already lives somewhere better.** Every PR body in this repo has
  a mandatory "What changed, and why" section, and every design decision that
  matters has a `docs/<FEATURE>.md` page or an ADR. The changelog entry was a
  third copy, written last, under conflict pressure.
- **Nobody read it forward.** An operator asking "what changed since my last
  update" reads the release notes on GitHub or `git log` between two tags.
  A 9 100-line file with no version headings answers that question worse than
  either.

## Decision

- `CHANGELOG.md` is deleted. Its history stays in git (`git log -- CHANGELOG.md`
  and any pre-2026-09-07 checkout).
- **The PR is the record.** A change's *what* and *why* are written once, in
  the PR body's "What changed, and why". A feature's design still gets its
  `docs/<FEATURE>.md`; an invariant change still gets its ADR.
- **The promotion PR's body is the release notes.** Promotions squash-merge
  `dev` into `main`, so GitHub's generated notes see exactly one PR per
  release — the promotion — and none of the `dev` PRs. `release.yml` therefore
  composes each release's notes as the promotion PR body first, then GitHub's
  generated list (via the `releases/generate-notes` API, the same text
  `--generate-notes` produces). The promoter writes that body for the operator
  who reads it: one bullet per `dev` PR with its *why*, and every breaking
  change or operator action stated plainly. The `dev` PR bodies stay one click
  away behind the numbers.
- **Breaking changes** are announced in the promotion body (and the `dev` PR
  that introduced them) and, where they touch an invariant, in the ADR.
  `docs/VERSIONING.md` says so in place of the changelog bullet.
- Nothing is added back that is edited by every PR. A per-release curated
  notes file, a `docs/CHANGES.md`, or a section in `README.md` would recreate
  the same conflict; the generated notes are the changelog.

## Consequences

- `AGENTS.md`, `CONTRIBUTING.md`, `docs/VERSIONING.md`, `docs/FEATURE-NOTES.md`
  and the PR template no longer ask for a changelog entry. The PR template asks
  instead that the title and the "why" be written for the release notes.
- Documents that pointed at "the CHANGELOG" for a historical detail now point
  at the repository history. ADRs that quote the old file as history are left
  as written; history is not drift.
- The CI docs-only classifier drops `CHANGELOG.md` from its list.
- Release notes are the promotion body plus the generated list
  (`release.yml`). Their quality is now a function of the promotion body and
  the `dev` PR titles, which is where review attention belongs. A promotion
  PR with an empty body ships a release whose only prose is the generated
  list; the workflow warns rather than blocks.

## Enforcement

`scripts/check_release_version_test.go` already refuses a hand-authored version
number and a number-keyed deprecation window in the operator-facing files. No
test refuses the file's return — a changelog is not a security invariant — but
its absence is visible in the tree and this record says why.
