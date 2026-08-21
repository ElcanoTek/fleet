# Sandbox image freshness — the max-age rebuild backstop

## The problem this fixes

`fleet update` used to rebuild the on-box sandbox image only when one of its
change gates fired: the bundle's `sandbox/Containerfile` hash changed, the
resolved image tag changed, or the tag was missing from the service user's
image store. Those gates are correct for *change* detection — but a healthy
production box with a stable bundle trips none of them, ever. The result,
observed in the field: boxes that ran `fleet update` regularly were still
serving sandbox images built 6–7 weeks earlier, on top of an equally old
`fedora-minimal` base.

That matters because a container image is frozen at build time. An unchanged
Containerfile does not stop the base layers and packages *inside* the built
image from aging and accumulating **published, already-fixed CVEs**. CI's
Grype gate (fail on a fixable CRITICAL) scans a **fresh** build of the
Containerfile — only an on-box rebuild ever brings a deployed box up to what
CI vouched for.

Two compounding gaps were closed:

1. **No rebuild ever fired on a stable bundle** — the gates only detected
   change, never staleness.
2. **Even when a rebuild fired, `podman build` ran without `--pull`**, so it
   reused whatever (equally stale) base image was already in the local store.
   The generic Containerfile's "base tracks `fedora-minimal:latest`, so every
   on-box rebuild picks up the current upstream security patches" intent
   didn't actually happen unless the operator pulled the base by hand.

## What shipped

### The max-age backstop in `scripts/update.sh`

A new gate branch fires when every change gate is quiet but the installed
image's creation time (read from the **service user's** store via the same
`sandbox_podman` seam every other probe uses) is `FLEET_SANDBOX_MAX_AGE_DAYS`
or more days old:

- **Default: 7 days.** Override per box with `FLEET_SANDBOX_MAX_AGE_DAYS` or
  per run with `fleet update --sandbox-max-age <days>`. `0` disables the
  backstop. Non-numeric values die up front (the value gates a multi-GB
  rebuild; a typo must not silently disable it).
- **Age-triggered rebuilds run `--no-cache`.** A cached rebuild against an
  unmoved base reproduces the *same* image with the same old creation date —
  the backstop would re-fire every update while refreshing nothing. Bypassing
  the cache re-runs every `RUN` layer, so unpinned `dnf`/`pip` installs pick
  up current package versions even when the base digest didn't move. Change-
  triggered rebuilds keep the layer cache (their trigger guarantees a real
  change).
- **Only a positive answer triggers.** An unreadable creation time (store
  unreachable, template unsupported) skips the age gate — the same
  act-only-on-a-positive-answer rule the existing absent/error store probe
  applies. No spurious multi-GB rebuilds off environmental failures.
- The existing fail-closed semantics are unchanged: a failed age-triggered
  rebuild leaves the (present, stale-but-serviceable) image in place with a
  warning, and the post-rebuild dangling-layer prune still runs.

### `--pull=newer` on every sandbox build

`scripts/build-sandbox-image.sh` (and `bootstrap.sh`'s inline systemd-path
build) now pass `--pull=newer` to `podman build`: podman re-checks the
registry and pulls the base only when upstream published a newer one. This is
offline-safe — podman suppresses the pull error when a local copy of the base
exists — and a digest-pinned `FROM` (the reproducible-build choice some client
bundles make) is naturally unaffected. `build-sandbox-image.sh` also grew
`--no-cache` (env `FLEET_SANDBOX_BUILD_NO_CACHE=1`), which `update.sh` uses
for its age-triggered rebuilds and operators can use directly for a manual
full refresh.

## What deliberately did NOT ship

- **No rebuild-on-every-update.** Rebuilding unconditionally would either be a
  no-op (cached, same image — false freshness) or a mandatory multi-minute
  `--no-cache` build on every `fleet update`, punishing boxes that update
  daily. The age threshold gives a bounded staleness guarantee at a bounded
  cost.
- **No background/scheduled rebuild.** Freshness is enforced at the moment the
  operator is already updating the box (the `fleet update` path); a box that
  never updates has bigger staleness problems than its sandbox image, and a
  standalone timer would add a second build path to keep coherent with the
  update gate's stamps and fail-closed rules.
- **Prebuilt `sandbox.image` bundles are untouched.** When the manifest
  resolves `sandbox.image` to a registry ref, the whole on-box build step
  still skips — keeping a *published* image fresh is the publisher's CI
  pipeline's job (see the reusable `publish-sandbox-image.yml` workflow),
  and this box's `fleet update` pulls whatever the ref points at.
- **No change to the image the running service uses mid-flight.** As before,
  a rebuilt image is picked up by new sandbox containers after the step-5
  service restart.

## Operator summary

| Knob | Default | Effect |
| --- | --- | --- |
| `FLEET_SANDBOX_MAX_AGE_DAYS` / `--sandbox-max-age <d>` | `7` | Rebuild the sandbox image during `fleet update` when the installed one is ≥ d days old; `0` disables |
| `FLEET_SANDBOX_BUILD_NO_CACHE=1` / `build-sandbox-image.sh --no-cache` | off | Force every layer to re-run (full package refresh) on a manual build |

A stale box needs nothing new: the next `fleet update` after this release
notices the old creation date and rebuilds. For an immediate manual refresh:

```sh
sudo FLEET_SANDBOX_BUILD_NO_CACHE=1 FLEET_CLIENT_CONFIG_DIR=<bundle> scripts/build-sandbox-image.sh
```
