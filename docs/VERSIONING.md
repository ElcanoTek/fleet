# Versioning and releases

**Short version: nobody ever types a fleet version number.** fleet ships from a
rolling release train, every green push to `main` is tagged automatically with
the date, and every build derives its own identity from git. There is no
`VERSION` file, no semver bump, and no release ceremony.

The reasoning is [ADR-0059](adr/0059-date-based-rolling-releases.md). This page
is the operational reference.

## The format

```
YYYY.MM.DD.N          tagged as v<version>
```

- `YYYY.MM.DD` is the **UTC** date the release was cut. A train whose names
  change shape with the releaser's timezone is not reproducible.
- `N` counts that day's releases **from 1**. Several a day is the normal case —
  `v2026.09.04.1`, `v2026.09.04.2`, `v2026.09.04.3` — not an overflow condition.

Examples: `v2026.09.04.1`, `v2026.09.04.12`, `v2026.12.25.1`.

**What the number promises: when the tree shipped. That is all.** CalVer carries
no compatibility signal, and fleet deliberately does not add one — see
[Breaking changes](#breaking-changes) below. fleet's actual compatibility
contract is the **HTTP API** version, which is versioned separately and
independently (`/v1`, `X-Fleet-API-Version`; see
[`api-versioning.md`](api-versioning.md)). Those two numbers have nothing to do
with each other, on purpose.

## How a release happens

1. Work lands on `dev` (fast lane CI).
2. A promotion PR squash-merges `dev` into `main` behind the full `CI gate`.
3. `CI` goes green on that push to `main`.
4. **[`release.yml`](../.github/workflows/release.yml) tags it.** It fires on
   `CI`'s completion, checks the run was a green *push* to `main`, computes the
   next unused ordinal for today, tags the exact SHA CI certified, and publishes
   a GitHub release whose notes are generated from the commits since the
   previous tag.

Nothing else is a release step. There is no release commit, no version-bump PR,
and no artifact upload — fleet's deploy path is a git checkout that
`fleet update` pulls and rebuilds in place, so the tag *is* the release.

Two properties worth knowing:

- **A red `main` gets no tag.** The day's ordinal advances at the next green
  push, leaving a visible gap. A tag on a tree the gate rejected would be worse
  than a gap.
- **Re-runs are idempotent.** If the commit already carries a release tag, the
  workflow says so and exits. It cannot open a second ordinal for one tree.

## Reading a build's identity

```console
$ fleet version
2026.09.04.2 (a1b2c3d4e5f6)
```

`fleet version` (and `fleet --version`) prints `<version> (<revision>)`. The same
string is reported by the chat health summary, `/healthz`, and the `fleet_version`
field of `/api-info`.

The version half is `scripts/version.sh describe`, stamped in at build time:

| What you see | What it means |
| --- | --- |
| `2026.09.04.2` | exactly the `v2026.09.04.2` release |
| `2026.09.04.2+3.g<sha>` | 3 commits past that release — a box tracking `main` between releases |
| `2026.09.04.2+3.g<sha>.dirty` | …and the working tree has uncommitted changes |
| `2026.09.04.2+dirty` | at the release, tree modified |
| `dev+g<sha>` | no release tag reachable (shallow clone, `--no-tags` fetch, or before the first release) |
| `dev` | unstamped build — a bare `go build`, `go run`, or `go test` |

The `dev` sentinel is deliberate: an unstamped binary says so instead of
claiming a release it was not built from.

> **Tags are load-bearing for builds.** A clone with no tags stamps `dev+g<sha>`.
> `scripts/update.sh` fetches `--tags` on every update for exactly this reason,
> and `scripts/bootstrap.sh` already did. If you build from a source tarball with
> no `.git`, you get `dev` — visibly unversioned rather than silently wrong.

## `scripts/version.sh`

The one place that knows the format. All four subcommands are read-only; only
`release.yml` ever writes a tag.

| Command | Output | Used by |
| --- | --- | --- |
| `scripts/version.sh describe` | `2026.09.04.2+3.g<sha>` | the Makefile's `-ldflags` stamp (the default subcommand) |
| `scripts/version.sh current` | `2026.09.04.2` | "what is the newest release?" |
| `scripts/version.sh next` | `v2026.09.04.3` | `release.yml`; run it locally to see what the next push would be named |
| `scripts/version.sh semver` | `2026.904.2` | Helm / npm, which reject four fields and leading zeros |

`make version` prints the `describe` value.

### The SemVer rendering

Helm chart versions and npm `version` fields are **parsed** and must be strict
3-field SemVer, which `YYYY.MM.DD.N` is not. `semver` re-renders the same
identity as `YYYY.<MM*100+DD>.<N>`:

| release | rendering |
| --- | --- |
| `2026.09.04.1` | `2026.904.1` |
| `2026.12.25.3` | `2026.1225.3` |
| `2027.01.05.2` | `2027.105.2` |

`MM*100+DD` avoids a leading zero (SemVer forbids `09`) while sorting in the same
order as the dates. It is a **rendering, not a second source of truth** — derived
one-directionally, never authored.

In-tree, `deploy/helm/fleet/Chart.yaml` and the two `package.json` files hold
`0.0.0` placeholders: neither the chart nor the web app is published separately,
so nothing consumes those fields in a checkout. `make helm-package` stamps the
real values when someone actually packages the chart.

## Breaking changes

CalVer cannot warn you that an upgrade is breaking, and fleet does not pretend
otherwise. A breaking change is announced in prose:

- a `CHANGELOG.md` entry saying plainly what breaks and what an operator must do;
- an **ADR** in [`adr/`](adr/) when it adds, weakens, or reverses an invariant
  (required by [`../AGENTS.md`](../AGENTS.md));
- for the HTTP API specifically, the `Deprecation` / `Sunset` / `Link` contract
  in [`api-versioning.md`](api-versioning.md) — that surface *does* have a
  version to bump, and it is the one operators integrate against.

## Deprecation windows are dated

A deprecation that waits for a release number waits forever on a train that
never cuts one. So every window is a **date**:

> removed in the first release on or after `YYYY-MM-DD`

That is a real clock: the release whose tag is `vYYYY.MM.DD.N` or later is the
one that drops it. ADR-0012's `fleet-admin` shim removal — previously "the first
release after 1.0.0", a condition that could never be met — is re-anchored this
way.

## What this replaced

For the record, so nobody reintroduces it:

- a top-level `VERSION` file containing `0.0.0`, described in `CHANGELOG.md` as
  "the single source of truth", which had never moved;
- a `CHANGELOG.md` claim that the project "aims to adhere to Semantic
  Versioning", over a file with exactly one heading (`[Unreleased]`) and ~8 400
  lines of shipped work beneath it;
- three further, mutually inconsistent version strings: the Helm chart's
  `0.1.0` / `"unreleased"`, `web/package.json`'s `0.1.0`, and a fabricated
  `"fleet_version": "1.2.0"` in the `/api-info` example;
- a `fleet-admin` removal trigger — "the first release after 1.0.0" — restated
  across eight files and unreachable in all of them.

`CHANGELOG.md` is still maintained, and is still where a user-visible change gets
explained (the PR template asks for it). What it no longer does is pretend to be
a semver release history: it is a running log, and the per-release notes are
generated from the commits between tags.
