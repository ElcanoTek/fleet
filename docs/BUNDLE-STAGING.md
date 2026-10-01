# Staging the default bundle for a bare install (#1655)

## What broke

A bare `--enable-service` install points `FLEET_CLIENT_CONFIG_DIR` at the
generic bundle inside the fleet checkout (`/opt/fleet/src/config/default`).
The sandbox bind-mounts that bundle's directories with an SELinux relabel
(`:z`), which rootless Podman can only perform on files the service user owns,
at a path the unit can write. The checkout is root-owned, and under
`fleet.service`'s `ProtectSystem=strict` it is read-only, so on every
SELinux-enforcing host each pool fill failed with `lsetxattr … read-only file
system` (podman exit 126). Found on the 2026-09-30 real deploy (RD2 in
[`MCP-CATALOG-STATUS.md`](MCP-CATALOG-STATUS.md)).

## What shipped

- **A service-owned copy.** `stage_default_bundle` (`scripts/lib/bundle.sh`)
  copies `config/default` to `/var/lib/fleet/bundle`, inside the unit's
  `StateDirectory`, so no new writable path is opened, and marks it with
  `.fleet-staged-from`, which names its source. Only the generic bundle is ever
  staged; a client bundle placed inside the checkout gets a warning instead.
- **bootstrap** stages it on a bare `--enable-service` install and points
  `FLEET_CLIENT_CONFIG_DIR` at it. If staging fails, bootstrap stops before the
  env file or the unit is written.
- **update** refreshes the copy from the checkout it just pulled, and moves a
  box installed before staging existed onto it. The path comes from the
  unit's `StateDirectory`, not the account's passwd home (the unit runs with
  `ProtectHome=yes`). A staged copy that has gone missing while the env file
  still points at it is recreated. An update whose bundle did not actually
  move says so instead of reporting a refresh.
- **doctor** fails a service still pointed at a checkout's generic bundle and
  names the repair. It checks that the bundle lies inside the unit's writable
  paths (`ReadWritePaths` and the state dir; a failure on an SELinux-enforcing
  host, advice elsewhere, since only there does the relabel run), and it
  checks ownership across the whole bundle tree, not just its top directory.

## Safety properties

- **No root writes into the service-owned tree.** That tree is controlled by
  the service account, so a root write there could be redirected by a planted
  symlink (the marker pointed at `/etc/fleet/fleet.env`, say). Root only
  *reads* the source, archiving it whole into a root-private temp file. Every
  write (unpack, sync,
  marker) runs as the service user via `runuser`. The marker is replaced, never
  written through.
- **A partial copy never reaches `rsync --delete`.** The archive is unpacked
  only after the tar that wrote it exited 0 (a stream could hand the owner a
  well-formed partial archive before a read failure was known), and the owner
  side also checks that `manifest.yaml` arrived before syncing.
- **Mounts stay valid under running sandboxes.** The copy is synced in place
  with rsync (files replaced, directories kept). There is no rename-and-delete
  fallback, which would empty the directories mounted into running
  containers, and no `restorecon`, which would reset the `:z` label under
  them.
- **A hand-placed bundle is never deleted.** A non-empty destination without
  the marker is refused (exit 2).

## Deviations and deferrals

- **rsync is required.** bootstrap installs it as part of `FLEET_DEPS`. On a
  box bootstrapped before that, staging is refused with an install hint
  (`dnf install rsync`), and update does not install it, keeping to its rule
  that it is an updater, not a provisioner.
- **The copy follows the checkout, not a release.** A refresh copies whatever
  the checkout holds after update's pull (the unpulled checkout under
  `--no-pull`).
- **A re-cloned checkout is not adopted automatically.** A copy whose marker
  names a different checkout is reported as not refreshed. Re-running
  bootstrap from the new checkout restages it.
- **Kubernetes is unaffected.** Under the Helm chart the bundle is baked into
  the images, and a sandbox pod mounts only the workspace claim, so there is
  no host checkout to relabel.
