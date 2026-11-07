# ADR-0101: Remove the on-box build; a box needs a published release

- **Status:** Accepted
- **Date:** 2026-11-07
- **Deciders:** fleet maintainers (owner direction)
- **Amends:** [ADR-0100](0100-signed-release-artifacts.md) (decision 9 kept
  `--from-source` for development boxes, marked
  for removal once boxes ran artifacts; this removes it)

## Context

ADR-0100 moved every box to signed release artifacts but kept the old on-box
build behind `--from-source` on `update.sh` and `bootstrap.sh`, for
development boxes, with every piece of it marked for deletion. It also kept a
shell-verifier fallback in `update.sh` for the first artifact update of a box
whose installed fleet predated `fleet release`.

The owner has since confirmed:

- all 12 fleet boxes run signed release v2026.11.07.21 with artifact-based
  updates;
- Go, Node, mercurial, `/usr/local/lib/fleet-node`, node-26, `web/node_modules`
  and the Go and npm caches have been removed from every box;
- all 11 client-config bundles dropped their `sandbox/Containerfile`, and every
  box runs the signed release sandbox image.

Nothing uses the build path or the fallback any more. Kept, they are code
that is never run and so never tested in practice, a second way to put an
unverified binary on a box, and a toolchain expectation that no box meets.

## Decision

1. **No on-box build of fleet.** The `--from-source` paths in `update.sh` and
   `bootstrap.sh` are deleted: the `make build` steps, the Go/Node presence
   checks (`require_go_toolchain`, `legacy_build_from_source`,
   `build_fleet_binary`), the from-source install (`fleet_install_built_binary`)
   and restart, and their dry-run lines.
2. **Asking for it fails clearly.** `--from-source`, `FLEET_UPDATE_FROM_SOURCE=1`
   and `FLEET_BOOTSTRAP_FROM_SOURCE=1` exit non-zero with a message that names
   this ADR and says what to do instead (install a release; build unreleased
   code on a development machine). The script does not quietly install a
   release in their place: whoever asked expected a build.
   `FLEET_UPDATE_FROM_SOURCE=0` is accepted and ignored, because an `update.sh`
   from before this change forwards it when it re-executes the new copy after
   a self-update. Both variables move to the config registry's *Retired*
   group.
3. **No shell-verifier fallback in `update.sh`.** An installed fleet without
   the `fleet release` verbs (a from-source install that never took a release)
   now stops the update with a message to re-run `bootstrap.sh
   --enable-service` with its original flags. Bootstrap is idempotent and
   installs the release over the existing install. `scripts/lib/release.sh`
   keeps verifying bootstrap's first binary and the signed manifest both
   scripts check before moving a checkout; `fleet_release_older`, used only by
   the removed fallback, is deleted.
4. **No release, or no network: stop.** Bootstrap needs a published release.
   When GitHub cannot be asked for the newest release (offline, rate-limited,
   none published), when origin cannot be fetched, or when the release does
   not download and verify, `update` and `bootstrap` stop, change nothing,
   and say that there is no build to fall back to. `--tag` still names a
   specific release.
5. **Doctor sweeps only what a release box makes.** The root-cache reclaim
   measured and cleaned the Go build/module and npm caches that on-box builds
   left behind as root. It now covers the dnf cache only, at a 1 GiB threshold.

**Kept, deliberately:**

- `make build`, `make web` and `GOTOOLCHAIN ?= auto` in the Makefile: CI,
  release.yml and developer machines build there.
- The on-box build of a bundle's **own** sandbox Containerfile (layered on the
  release image via `ARG FLEET_SANDBOX_BASE`). It is a supported bundle
  feature, needs only podman, and builds the bundle's image, not fleet. No
  bundle uses it today; nothing marked it for removal.
- `scripts/build-sandbox-image.sh`, including its fallback to the generic
  Containerfile for a bundle that ships none. On a box it now runs only for a
  custom Containerfile; the fallback serves by-hand runs, CI and release.yml.
- `fleet cleanup`'s `go clean` when a Go toolchain is present: harmless on a
  release box (there is none) and useful on a developer machine.
- `--artifact` on `update.sh`, now a no-op, so scripts that pass it keep
  working.

## Enforcement

- `internal/admincli/scripts_dryrun_test.go` `TestOnBoxBuildIsGone`: the flag
  and both env inputs are refused with the ADR-0101 message;
  `FLEET_UPDATE_FROM_SOURCE=0` and `--artifact` are accepted; `update.sh`,
  `bootstrap.sh` and `lib/release.sh` run no `make build` and name none of the
  removed helpers, `GOTOOLCHAIN`, or the old marker.
- `internal/admincli/doctor_test.go`: the dnf-only reclaim and its threshold.
- `internal/config/envreg`: both variables are in the Retired group, and
  `docs/CONFIGURATION.md` is regenerated from it.

## Consequences

- A box can only ever run a binary release.yml signed. There is no
  "build it here" escape hatch for an outage. During a GitHub outage, the
  running release keeps running and `fleet release rollback` still works
  offline (it swaps `fleet.prev` back).
- Testing unreleased code means building it elsewhere and waiting for a
  release, or running it on a development machine. A development box that
  wants to track `main` between releases must do so outside these scripts.
- `update.sh` and `bootstrap.sh` lose about 150 lines between them, and every
  remaining install path goes through `fleet release install`.

## Alternatives considered

- **Keep `--from-source` for development boxes.** No development box uses
  it, and keeping it means keeping a Go/Node expectation and an unverified
  install path in scripts that run as root on production boxes.
- **Treat `--from-source` as a no-op and install a release.** Silent
  substitution hides a mistake: the caller asked for something else.
- **Drop the env variables from the registry entirely.** The scripts still
  read them, to refuse them, and the registry should list every variable a
  script reads. Retired says what they did and why they stop now.
