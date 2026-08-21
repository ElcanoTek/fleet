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

## Follow-up: two ways the backstop could silently not run

Shipping the backstop exposed two paths on which it reported success without
ever having run. Both are closed:

### The self-update re-exec dropped flag state

`update.sh` re-execs the copy the pull just installed when the update changed
`update.sh` itself (bash holds the pre-update inode, so a fix to the updater
would otherwise only take effect one update later). That re-exec passes **no
argv** — every setting a flag can change has to be restated as its env
equivalent on the `exec env` line — and `--sandbox-max-age` was not on it, so
it was silently downgraded to the default `7` on exactly the run that pulled
the fix: the one run an operator gets only once. `--adopt-units` and
`--no-timers` were dropped the same way.

All three are forwarded now (`FLEET_SANDBOX_MAX_AGE_DAYS`,
`FLEET_UPDATE_ADOPT_UNITS`, `FLEET_UPDATE_OFFER_TIMERS`). The deliberate
non-forwards are documented at the call site: `--no-pull` (forwarding it would
skip the client-bundle pull the re-exec exists to preserve), `--dry-run` (never
reaches the re-exec — it skips the fast-forward), `--src` (re-derives from the
script path the exec names) and `--yes` (hardcoded to 1: the operator already
confirmed this commit range). `TestUpdateReexecForwardsFlagState` derives the
flag-settable variables from the arg parser itself, so a NEW flag fails the
test until it is either forwarded or exempted with a reason.

### An unresolvable tag read as a clean bill of health

Both store-aware gates need a resolved ref: the presence probe and the age
backstop. When `build-sandbox-image.sh --print-tag` returned nothing, the empty
ref fell out of the gate's if/elif chain with no build reason set, and the step
printed `sandbox image up to date (…, tag unresolved)` — a pass for an image
nothing had looked at. That is exactly how a weeks-old sandbox survives on a
box that updates cleanly every day.

The empty-ref case is now its own branch. It sets a `sandbox_unverified`
reason, and the step reports that as a **warning** naming what was skipped (the
presence check *and* the n-day backstop), how to diagnose the tag, and the
by-hand rebuild — never as "up to date". An inconclusive store probe (podman
exit 125) reports through the same single path instead of a warning followed by
a reassuring `ok`. `--print-tag`'s stderr is captured rather than discarded so
the warning can name the cause: since `--print-tag` always prints a `name:tag`
when it runs at all (falling back to `localhost/fleet-sandbox:latest` when the
manifest names no `sandbox.tag`), an empty answer means the resolver script
itself could not run.

Neither case is fatal. An unresolvable tag is no evidence the running image is
broken, and dying there would strand an otherwise good update — the fail-closed
`die` stays where it belongs, on a *failed build* whose ref is not known to
exist in the service user's store.

### Related: `podman images` as root is the wrong store

Not a bug, but the reason a working rebuild can look like it did nothing.
`build-sandbox-image.sh` builds as the unit's `User=` (rootless), and every
probe here — presence, age — reads that account's store. `sudo podman images`
shows **root's** store, which on a box provisioned before that change can still
hold stale `localhost/fleet-sandbox*` images that `fleet update` will never
touch again. Read the store that matters:

```sh
sudo install -d -o fleet -g fleet -m 0700 /run/fleet
sudo runuser -u fleet -- env HOME="$(getent passwd fleet | cut -d: -f6)" \
  XDG_RUNTIME_DIR=/run/fleet podman images
```

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
