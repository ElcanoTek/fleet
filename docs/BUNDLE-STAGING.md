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
- **doctor** reports a service still pointed at a checkout's generic bundle
  and names the repair. It checks that the bundle lies inside the unit's
  writable paths (`ReadWritePaths` and the state dir), and it checks ownership
  across the whole bundle tree, not just its top directory. Each of these is
  a failure on an SELinux host, enforcing or permissive (permissive mode still
  relabels), and advice only where SELinux is disabled or absent, where no
  relabel is attempted and a world-readable bundle mounts fine.
- **`fleet update --check`** calls a staged copy current only when its marker
  names this checkout's `config/default` (both paths resolved, so a relative
  or symlinked `FLEET_ROOT` matches) *and* its content still matches that
  source, file for file (bytes, entry types, link targets and the owner's
  permission bits, which the refresh restores). A checkout fast-forwarded outside `fleet update`
  leaves the marker naming the same path over old bytes, so that copy is
  reported stale. The comparison reads the service-owned copy through an
  `os.Root`, so no symlink in it can lead the root-run check out of the tree.
  A marker that is there but unusable (a link, a FIFO, empty) is reported
  stale, since update.sh would not recognise the copy. A copy the invoking
  user cannot read (the shipped unit's state dir is `0700`) is reported as
  unknown and fails the check, with the `sudo` re-run to use. A bundle with no
  marker at all is indistinguishable from a hand-placed one and is checked as
  that (update leaves those alone too).

## Safety properties

- **No root writes into the service-owned tree.** That tree is controlled by
  the service account, so a root write there could be redirected by a planted
  symlink (the marker pointed at `/etc/fleet/fleet.env`, say). Root only
  *reads* the source, archiving it whole into a root-private temp file. Every
  write (unpack, sync,
  marker) runs as the service user via `runuser`. The marker is replaced, never
  written through.
- **The marker is never read through a link.** It sits in the service user's
  tree, so doctor and update open it with `O_NOFOLLOW` (`dd iflag=nofollow`)
  as the service user, and `fleet update --check` opens it with `O_NOFOLLOW`
  and requires a regular file. A link to a root-only file or to the running
  service's `/proc/<pid>/environ` is refused at open time.
- **A partial copy never reaches `rsync --delete`.** The archive is unpacked
  only after the tar that wrote it exited 0 (a stream could hand the owner a
  well-formed partial archive before a read failure was known), and the owner
  side also checks that `manifest.yaml` arrived before syncing.
- **A failed refresh leaves the previous copy.** A tree the service user
  cannot fully rewrite (a root-owned file from a hand edit) is refused before
  anything moves. Otherwise the current copy is kept aside first, and if the
  in-place sync fails part-way (a full disk, an I/O error), the kept copy is
  put back in place. If putting it back fails too (the same full disk), the
  copy is partly updated: the kept copy is then left on disk, its path is
  printed, and update stops before restarting the service rather than
  restarting onto a half-written bundle. The new marker is written before anything changes and
  renamed into place after the sync, so a full disk cannot leave a copy
  without one. rsync compares content (`--checksum`), so an edit that
  keeps a file's size and mtime is still picked up.
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
