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

- **`main` is the only branch** ([ADR-0062](docs/adr/0062-trunk-based-development.md)).
  Branch off the latest `main` with a short prefix (`feat/…`, `fix/…`,
  `docs/…`) and open the PR against `main`. Every PR is **squash-merged**, so
  one PR is one commit on `main` and one release.
- One change per PR. A PR that does one thing is easier to review, revert, and
  merge alongside everyone else's; a grab-bag conflicts with all of them.
- Fill in the PR template rather than deleting it: what changed and why, what
  you actually ran to verify it, and the scope and deviations. The title and
  the "why" prefill the squash commit message, which is the release notes, so
  write them for the operator who reads those. There is no changelog file
  ([ADR-0061](docs/adr/0061-retire-the-changelog.md)).
- Every PR waits for a human to merge. Nothing merges itself.
- After it is open, the PR is driven to green by whoever holds it — red
  checks, review threads, conflicts, whatever caused them. The procedure is
  the steward skill, [`.agents/skills/steward/SKILL.md`](.agents/skills/steward/SKILL.md),
  with the reference behind it in
  [`docs/PR-STEWARDSHIP.md`](docs/PR-STEWARDSHIP.md).

Commit subjects are imperative ("Add X", not "Added X"); the body explains
*why* when the diff does not. Every PR runs the full `CI` gate — the `-race`
lane, govulncheck, Grype and both Playwright suites included — so a green PR
is a shippable change, and merging it is what ships it.

### Releases (there is nothing to do)

Every PR that merges green into `main` is tagged `vYYYY.MM.DD.N` by the
`Release` workflow and published as a GitHub release. **The squash commit
message is the release notes.** `release.yml` publishes the released commit's
message verbatim, then GitHub's generated PR list. GitHub prefills the squash
dialog with the PR's title and body, so write the body's "What changed, and
why" for the operator who reads the release — what changed, the *why*, and any
breaking change or operator action stated plainly (ADR-0061 retired the
changelog on this basis). In the dialog, keep that section and drop the rest —
verification, scope, the checklist, any tool's footer. What is in the box when
you click merge is what ships. (Merging through the API instead? Pass the same
text as the commit message.) Only the newest release is listed on the Releases
page; tags are permanent, and an older release's notes are its commit message.

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
