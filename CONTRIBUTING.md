# Contributing to fleet

Thanks for your interest in fleet. This page is the short front door; the
detail lives in three places, and this file deliberately does not repeat them:

- **[`AGENTS.md`](AGENTS.md)** — the operating guide for everyone working in
  the tree, human or agent: what fleet is, the build/test/lint targets and why
  the `fleet_host_executor` tag matters, the two CI lanes, the non-negotiable
  invariants, the conventions. Read it first.
- **[`ONBOARDING.md`](ONBOARDING.md)** — clone to a passing
  `make test` and one real sandboxed chat turn, in one linear path.
- **[`docs/README.md`](docs/README.md)** — the docs index: a curated
  by-question list, then every page under `docs/` A–Z (a test keeps that
  list complete).

## Getting set up

Prerequisites are Go (the version in `go.mod`), Node (the major in
`web/.nvmrc`), rootless Podman for the sandbox-backed tests, and PostgreSQL for
the store suites. `ONBOARDING.md` walks through installing them; the
Makefile is the source of truth for every command (`make help`).

Before opening a PR: `make build && make lint && make test && make ci-web`
(or `make ci-local`, the whole gate). `make build` is not decoration: lint and
tests compile with the `fleet_host_executor` tag, the release binary
deliberately does not, and only `make build` / `make compile` checks that
tree. Tests skip silently without a database — set `DATABASE_URL` and
`FLEET_TEST_DATABASE_URL` and check with `-v` that the suites you care about
print `PASS`, not `SKIP`.

## Branches and pull requests

- Feature work targets **`dev`**. Branch off the latest `dev` with a short
  prefix (`feat/…`, `fix/…`, `docs/…`); `main` receives only promotions.
- One change per PR. A PR that does one thing is easier to review, revert, and
  merge alongside everyone else's; a grab-bag conflicts with all of them.
- Fill in the PR template rather than deleting it: what changed and why, what
  you actually ran to verify it, and the scope and deviations. The title and
  the "why" are what the promoter carries into the release notes, so write
  them for the operator who reads those. There is no changelog file
  ([ADR-0061](docs/adr/0061-retire-the-changelog.md)).
- Every PR waits for a human to merge. Nothing merges itself.
- After it is open, the PR is driven to green by whoever holds it — red
  checks, review threads, conflicts, whatever caused them. The procedure is
  the steward skill, [`.agents/skills/steward/SKILL.md`](.agents/skills/steward/SKILL.md),
  with the reference behind it in
  [`docs/PR-STEWARDSHIP.md`](docs/PR-STEWARDSHIP.md).

Commit subjects are imperative ("Add X", not "Added X"); the body explains
*why* when the diff does not. A `dev` PR runs the fast CI lane; the
promotion to `main` is the first time the full gate (the `-race` lane,
govulncheck, Grype, both Playwright suites) sees the code.

### Promotions (dev → main)

Feature work merges into `dev` (the fast lane); `dev` is promoted to `main`
via a **squash**-merge PR — the squash titles are the promotion log.

**The promotion's squash commit message is the release notes.** `release.yml`
publishes the released commit's message verbatim, then GitHub's generated PR
list (which sees only the promotion, never the `dev` PRs behind it). GitHub
prefills the squash dialog with the promotion PR's title and body, so write the
body's "What changed, and why" for the operator who reads the release: one
bullet per `dev` PR with its number and one-line *why*, and any breaking change
or operator action stated plainly (ADR-0061 retired the changelog on this
basis). In the dialog, keep that section and drop the rest — verification,
scope, the checklist, any tool's footer. What is in the box when you click
merge is what ships. (Merging through the API instead? Pass the same text as
the commit message.)

Squashing
has one structural side effect: the branches' merge-base never advances, so
any region dev changes, promotes, and later changes again would read as
both-sides-modified (a spurious conflict) on the next promotion PR.

The fix is an **ancestry merge** recorded on `dev` right after each promotion:
`git merge -s ours main` makes main an ancestor of dev while changing nothing
in dev's tree. The `Promotion ancestry` workflow
([`.github/workflows/promotion-ancestry.yml`](.github/workflows/promotion-ancestry.yml))
does this automatically on every push to `main`, gated on **tree identity** —
`-s ours` is only provably safe when `main^{tree}` is byte-identical to
`dev^{tree}`, i.e. main carries nothing dev lacks. If the trees differ (dev
moved mid-promotion), the workflow fails loudly instead of guessing.

If it fails and you have looked, the manual step it automates is:

```bash
git fetch origin main dev
# Precondition — both must print the same tree hash:
git rev-parse origin/main^{tree} origin/dev^{tree}
git checkout -B dev origin/dev
git merge -s ours --no-ff origin/main -m "Merge main back into dev after promotion (ancestry only; tree unchanged)"
git push origin dev
```

If the trees differ, do **not** use `-s ours` — resolve the divergence for
real first (dev commits that landed mid-promotion are content main genuinely
lacks; `-s ours` from main's side would be wrong, and from dev's side it is
only safe once you have confirmed main brings nothing new).

### Releases (there is nothing to do)

Every promotion that goes green on `main` is tagged `vYYYY.MM.DD.N` by the
`Release` workflow and published as a GitHub release with generated notes.
Never add a hand-authored version number anywhere, and date every deprecation
window ("removed in the first release on or after `YYYY-MM-DD`") rather than
keying it to a release number that will never be cut. Details and the
reasoning: [`docs/VERSIONING.md`](docs/VERSIONING.md), ADR-0059.

## Reporting bugs and proposing features

Open a GitHub issue with enough context to reproduce or understand the
request. For a security vulnerability do **not** open a public issue — see
[`SECURITY.md`](SECURITY.md). Conduct: [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).

## License

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE) that covers this project.
