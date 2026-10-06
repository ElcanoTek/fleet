# ADR-0078: Track the latest node major; install the signed upstream build when the distro lags

- **Status:** Accepted
- **Date:** 2026-10-06
- **Deciders:** fleet maintainers
- **Supersedes:** the "production should track Active or Maintenance LTS, never
  Current" rule recorded in `.github/dependabot.yml` and enforced by the node
  LTS reminder of #1300 (`node-lts-reminder.yml`, now `node-latest-reminder.yml`).

## Context

`web/.nvmrc` is the single declaration of the node major. CI, bootstrap,
doctor and update all read it. Until now the rule for moving it was "Active or
Maintenance LTS, never Current", for two reasons:

1. **Odd majors never became LTS.** v25 lived seven and a half months, so a
   bump to it would strand the tree on an unsupported line.
2. **Fedora packages a new major on its own schedule.** bootstrap and
   `doctor --node` install the versioned dnf stream (`nodejs<major>`). Moving
   `.nvmrc` ahead of that stream left fresh boxes on the unversioned
   fallback, and made `fleet update` refuse to build, because `update.sh`
   hands a node shortfall to `doctor --node` and refuses if it stays short.

The rest of fleet runs on the latest toolchains. Go is pinned in `go.mod` to
the newest release, and `GOTOOLCHAIN=auto` fetches it, verified, when the
distro `golang` is behind. Node was the exception, held back mostly by reason 2.
As of this ADR, node 26 (Current since May 2026, LTS from 2026-10-28) has a
`nodejs26` RPM in Fedora 45 only. Fedora 43 and 44, where today's boxes run,
carry nothing newer than `nodejs24`.

## Decision

**fleet tracks the latest released node major.** `web/.nvmrc`,
`engines.node` and the `@types/node` major move together, as soon as a new
major ships. `node-latest-reminder.yml` files an issue when nodejs.org
publishes a major newer than `.nvmrc`.

**When the distro has no `nodejs<major>` stream, bootstrap and
`doctor --node` install the official nodejs.org release instead**
(`fleet_node_tarball_install` in `scripts/lib/node-version.sh`):

- The newest `v<major>.x` `SHASUMS256.txt.asc` is verified with `gpgv`
  against node's release keys, which are **vendored** in
  `scripts/lib/node-release-keys.asc` (fingerprints in `.list`, regenerated
  from nodejs/release-keys by `scripts/update-node-release-keys.sh`). `gpgv`
  trusts only that keyring: nothing in root's `~/.gnupg` counts, and no
  keyserver is consulted.
- The version and the sha256 are read from `gpgv --output`, the signed text,
  so lines outside the signed block cannot steer them. The tarball's sha256
  must match that text before anything is unpacked.
- The release unpacks under `/usr/local/lib/fleet-node/`. `node-<major>`,
  `npm-<major>` and `npx-<major>` are linked into `/usr/local/bin`, using the
  versioned names Fedora's streams use, so the existing resolver
  (`fleet_resolve_node_bin`, `fleet_resolve_npm_cli`), the npm interpreter pin
  and the `FLEET_NODE_BIN` stamp all work unchanged. `/usr/bin` is searched
  first, so once the distro ships the stream, the RPM wins with no further
  action.
- dnf does not patch this install, so every full `fleet doctor` run (not
  `--check`) re-runs the signed install, which is a no-op when the newest
  `v<major>.x` is already present, and restarts the tier if it moved.
- The installer never moves backwards. An older release is still validly
  signed, so a replayed or stale manifest offering a lower patch than the
  installed `node-<major>` is refused instead of relinked.
- `doctor --node` installs `gnupg2` (for `gpgv`) before trying the fallback.
  A box provisioned before this change may not have it, and without it
  `fleet update` would still be stranded.
- `scripts/update-node-release-keys.sh` refuses a keyring whose primary keys
  are not exactly `keys.list`, and `TestVendoredNodeReleaseKeysMatchTheirList`
  pins the committed pair. `gpgv` trusts every primary in the file, so an
  unlisted one would be a signer no reviewer saw.
- Any verification failure installs nothing and reports why. The box then
  stays exactly where the dnf-only path would have left it.

## Consequences

- Moving to node 26 no longer waits on Fedora. An F44 box's next
  `fleet update` installs the upstream node 26 through `doctor --node`. A
  fresh bootstrap does the same.
- This adds a root-run download path outside the package manager. Its trust
  root is the vendored keyring, which goes stale when node adds a releaser.
  The failure mode is fail-closed: a release signed by an unknown key is
  refused. The weekly reminder files an issue when
  nodejs/release-keys' `keys.list` drifts from ours.
- Upstream-tarball node is patched by `fleet doctor`, not by `dnf upgrade`.
  A box that runs neither stays on the release it installed, just as an RPM
  box that never runs `dnf upgrade` would.
- The odd-major objection is retired along with the rule. Per
  nodejs/Release's `schedule.json`, v27 has an alpha phase, releases in
  April 2027 and becomes LTS, so from 27 on every major is an LTS line. If
  that changes, this ADR is the place to revisit.

## Not done

- No pruning of older unpacked releases under `/usr/local/lib/fleet-node/`.
  A refresh leaves the previous patch release on disk. That is about 200 MB
  per release, and it is safe to delete once the tier has restarted.
- Boxes without dnf do not get the fallback. Both bootstrap and doctor take
  it only after dnf has failed to provide the stream. fleet's install targets
  Fedora, and on other distros node is still the operator's job, as before.
