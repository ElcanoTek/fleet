# ADR-0059: Date-based rolling releases; every green push to `main` is tagged automatically

- **Status:** Accepted
- **Date:** 2026-09-04
- **Deciders:** fleet maintainers

## Context

fleet's release identity was a top-level `VERSION` file containing `0.0.0`,
declared in `CHANGELOG.md` as "the single source of truth", stamped into both
binaries by the Makefile via `-ldflags -X`. Around it, `CHANGELOG.md` claimed the
project "aims to adhere to [Semantic Versioning]".

Neither claim survived contact with how fleet actually ships:

- **No release had ever been cut.** `git tag` returned nothing, no GitHub release
  existed, `VERSION` had never moved off `0.0.0`, and `CHANGELOG.md` had exactly
  one heading — `[Unreleased]` — over ~8 400 lines of shipped work.
- **The number could not mean anything.** SemVer encodes a compatibility promise
  between releases. fleet's deployment model (ADR-0012) is a git checkout that
  `fleet update` pulls and rebuilds in place: operators run the tip of `main`,
  there are no maintained release branches, and no release artifacts are
  published to compare against. A number promising compatibility between
  releases nobody could name or fetch was decoration.
- **It had already rotted in four directions.** `VERSION` said `0.0.0`, the Helm
  chart said `version: 0.1.0` / `appVersion: "unreleased"`, `web/package.json`
  said `0.1.0`, and `docs/api-versioning.md` illustrated the `/api-info` payload
  with a fabricated `"fleet_version": "1.2.0"`. Four independent strings, one of
  which claimed to be the only one.
- **It armed a trigger that could never fire.** ADR-0012 pinned the
  `fleet-admin` shim's removal to "the first release after 1.0.0" — a condition
  restated across eight files, and unreachable on a project that never cuts a
  numbered release. A deprecation window with no clock is not a window.
- **Nothing identified a build.** An operator asking "which fleet is this box
  running?" got `0.0.0 (4e87891a2b3c)` — the revision was the only informative
  half, and `fleet update --check`'s "commits behind" count existed precisely
  because the version could not answer.

fleet ships continuously: `dev` integrates, promotions squash-merge into `main`
behind the full `CI gate`, and boxes track `main`. Several promotions in one day
is normal. That is a rolling release train, and the honest thing for a rolling
train is to stop pretending each push is a curated, numbered release — while
still giving every shipped tree a name an operator can say out loud.

## Decision

**fleet has no hand-maintained version number. Releases are date-based (CalVer)
and tagged automatically; a human never types a version.**

1. **The format is `YYYY.MM.DD.N`**, tagged `v<version>` — e.g. `v2026.09.04.1`,
   `v2026.09.04.2`. The date is **UTC**; `N` counts that day's releases from 1,
   because several a day is the expected case, not an overflow. The format
   carries **no compatibility promise** — it says *when* a tree shipped, which
   is the only thing a rolling train can honestly assert.
2. **The tag is the release.** There is no release commit, no version-bump PR,
   no changelog curation step, and no build artifact to publish: the deploy path
   is still a git checkout that rebuilds in place. Creating the tag is therefore
   the entire release process.
3. **`.github/workflows/release.yml` creates it, on every green push to
   `main`.** It fires on `workflow_run` completion of `CI`, gated on
   `conclusion == 'success' && event == 'push' && head_branch == 'main'`, tags
   the exact SHA that CI certified, and publishes a GitHub release whose notes
   are generated from the commits since the previous tag. A push whose CI is red
   gets **no** tag, and nothing in the workflow can be triggered by hand to
   release a red tree.
4. **`scripts/version.sh` is the only thing that knows the format** — `current`,
   `describe`, `next`, `semver`. The `VERSION` file is deleted.
5. **Builds derive their identity from git, not from a file.** The Makefile
   stamps `scripts/version.sh describe` into `internal/version.version`:
   `2026.09.04.1` at a release, `2026.09.04.1+3.g<sha>` three commits past one
   (which is what a box tracking `main` between releases honestly is), plus a
   `.dirty` marker for a modified tree, and the pre-existing `dev` sentinel when
   no tag is reachable. An operator can now read "how far past which release am
   I" out of one string.
6. **Deprecation windows are dated, not numbered.** Anything that previously
   waited for a release number waits for a **date** instead — ADR-0012's shim
   removal is re-anchored to "the first release on or after 2026-12-01".
7. **The two ecosystems that parse a version field get a rendering, not a second
   source of truth.** Helm chart versions and npm package versions must be
   strict 3-field SemVer, which `YYYY.MM.DD.N` is not (four fields; leading
   zeros). `scripts/version.sh semver` re-renders the same identity as
   `YYYY.<MM*100+DD>.<N>` (`2026.09.04.1` → `2026.904.1`) — order-preserving,
   derived, never authored. In-tree those files hold `0.0.0` placeholders,
   because neither the chart nor the web app is published separately; `make
   helm-package` stamps the real value when someone actually packages the chart.

## Enforcement

- `scripts/version.sh` — sole owner of the format; `scripts/version_test.go`
  covers `describe`/`next`/`semver` against synthetic tag histories, including
  the multiple-releases-per-day ordinal and the zero-padded-month rendering.
- `scripts/check_release_version_test.go` — asserts no `VERSION` file returns,
  that the Makefile stamps from `scripts/version.sh` rather than a file, that
  release.yml stays gated (green + push + main) and idempotent, that the Helm and
  npm placeholders stay placeholders in sync with their lockfiles, and that every
  file carrying the `fleet-admin` deprecation window states the DATED form.
- `.github/workflows/release.yml` — the only writer of release tags; idempotent
  (an `--exact-match` describe short-circuits a re-run) and serialized by a
  non-cancelling concurrency group.
- `Makefile` (`VERSION := $(shell scripts/version.sh describe)`) — the one
  stamping path, used by `bins`, `fleet-bench` and `helm-package`.
- `internal/version` + `internal/version/version_test.go` — the "dev" sentinel
  for unstamped builds stays, so a bare `go build` is still honestly labelled.

## Consequences

**Easier.** Releasing is a no-op: merge, and the tree that passed CI acquires a
name. Every shipped tree has one, including several in a day. "Which build is
this box running?" has a real answer, and the answer says how stale it is. There
is no version-bump PR to review, no semver argument to have, and no rotting
number in a file nobody remembers to move. Deprecation windows have actual
clocks.

**Harder / accepted costs.**

- **No compatibility signalling.** CalVer cannot warn that an upgrade is
  breaking. fleet never actually offered that (there were no releases to be
  compatible with), but the option is now explicitly off the table: a breaking
  change must be announced in `CHANGELOG.md` and, where it changes an
  invariant, an ADR — prose, not a major-version bump. The **HTTP API** keeps
  its own independent version (`/v1`, `X-Fleet-API-Version`,
  `docs/api-versioning.md`), which is where fleet's real compatibility contract
  lives and which this ADR deliberately does not touch.
- **Tags are load-bearing for builds.** A shallow clone, or one fetched with
  `--no-tags`, stamps `dev+g<sha>` instead of a version. `scripts/update.sh`
  therefore fetches `--tags` explicitly rather than relying on git's
  tag-following (`scripts/bootstrap.sh` already did); `scripts/fleet-upgrade.sh`
  never pulls, so it inherits whatever the checkout has. This is a real
  regression risk for anyone building from a source tarball — the `dev` sentinel
  makes it visible rather than silently wrong.
- **A red `main` gets no tag.** Deliberate: the ordinal for that day simply
  advances at the next green push, leaving a visible gap. We prefer a gap over a
  tag on a tree the gate rejected.
- **Two renderings of one identity.** `2026.904.1` next to `2026.09.04.1` is a
  seam someone will trip over. It is mechanical, one-directional, and confined
  to `scripts/version.sh semver` plus the two packaging call sites.

## Alternatives considered

- **Keep SemVer, actually cut releases.** Honest, but it buys a compatibility
  promise fleet has no way to honour: one deployment model (track `main`), no
  release branches, no published artifacts, no backports. The number would
  encode a policy nobody implements — the failure mode we are leaving.
- **`0.x` forever with a bumped patch per merge.** Still a hand-authored number
  in a file, still a bump to forget, and `0.0.517` tells an operator strictly
  less than a date does.
- **`git describe` with no tags at all** (pure `<sha>` identity, no releases).
  Simplest possible scheme, and rejected because a bare SHA is not sayable: "are
  you on `2026.09.04.2`?" is a question a human can ask over a phone, and
  release notes need a boundary to be generated between.
- **`YYYY.MM.DD` with no ordinal.** Breaks on the normal case — several
  promotions in a day would collide on one tag.
- **`YYYY.MM.DD-N` (SemVer pre-release) or `YYYY.MM.DD+N` (build metadata)** so
  the tag itself parses as SemVer. Both order wrongly under SemVer rules: a
  pre-release sorts *below* the release it hangs off, and build metadata is
  explicitly ignored in precedence. A version that sorts wrong is worse than one
  that needs a rendering for two file formats.
- **Tag on a schedule (nightly) rather than per push.** Decouples the tag from
  the gate that certifies it: a nightly tag can land on a tree that is green
  only by accident of timing, and a day with three promotions gets one name for
  three different trees.
- **A job inside `ci.yml` instead of a `workflow_run` workflow.** It would need
  `needs: [ci-gate]`, and `scripts/check_gate_needs_test.go` requires every job
  in `ci.yml` to appear in `ci-gate`'s own `needs` — a cycle. That test is the
  more valuable of the two rules (it is what prevents a red-but-not-required
  job), so the release moved out of the file instead.
