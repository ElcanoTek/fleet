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
- **The squash commit message is the release notes.** `release.yml`
  publishes the released commit's message (`git log -1 --format=%B`)
  prepended to GitHub's generated list (`gh release create --generate-notes`
  with a body does exactly that prepend). This is the shape openai/codex
  ships with, and it is deliberately the simplest one: the message is the
  text approved in the merge dialog, prefilled from the PR title and body, so
  there is no PR lookup and no parsing — what is in the box when
  the merge button is clicked is what ships. (A first cut copied the PR body
  through the API and then tried to filter coding-agent footers out of it;
  three review rounds of edge cases in an afternoon showed that parsing was
  the wrong layer.) The author writes that text for the operator who reads
  it: what changed, the *why*, and every breaking change or operator action
  stated plainly. (Written when promotions still existed — "one bullet per
  `dev` PR"; since ADR-0062 every PR merges into `main` directly and the same
  rule applies to each.)
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
- Release notes are the squash commit message plus the generated list
  (`release.yml`). Their quality is now a function of what the merger leaves
  in the merge dialog, which is where review attention belongs. A PR merged
  with a bare title ships a release whose only prose is that title and the
  generated list; the workflow never blocks on it.
- Only the newest release object is kept (`release.yml` prunes the rest after
  publishing; tags stay). This is what makes "the commit message is the
  notes" load-bearing rather than cosmetic: `git log` between two tags is the
  complete history, and the Releases page is a view of the current release,
  not an archive. Best-effort (a failed prune warns and the next run
  retries) and deliberately lossy in two respects: the generated half of an
  old release's notes (merged-PR list, compare link) is only reconstructible
  from the tags, and the releases published before this mechanism landed
  (`v2026.09.07.2` and earlier) took their prose from the promotion PR body,
  for which the PR itself is the record.

## Enforcement

`scripts/check_release_version_test.go` already refuses a hand-authored version
number and a number-keyed deprecation window in the operator-facing files. No
test refuses the file's return — a changelog is not a security invariant — but
its absence is visible in the tree and this record says why.
