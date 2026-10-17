# Bootstrap host hygiene: no Cockpit, a real hostname

Two small steps `scripts/bootstrap.sh` runs on a new box. `fleet update` runs
neither.

## What shipped

**Cockpit is uninstalled.** The Vultr Fedora image ships Cockpit's
root-capable web login on :9090, and fleet boxes are administered over ssh only.
When `rpm -q cockpit-ws` succeeds, bootstrap runs `dnf -y remove 'cockpit*'`
(`cockpit_uninstall`, `scripts/lib/cockpit.sh`). `FLEET_KEEP_COCKPIT=1` skips it
with one info line. The step is silent where rpm or Cockpit is absent, never
touches firewalld, prints the command under `--dry-run`, and only warns when dnf
fails.

**The box is named after its domain.** Boxes kept Vultr's `vultr.guest`, so
every prompt read `root@vultr`. With `--enable-web --domain <fqdn>`, bootstrap
runs `fleet_set_hostname` (`scripts/lib/hostname.sh`):

- It renames only when `hostnamectl --static` is exactly `vultr.guest`,
  `localhost` or `localhost.localdomain`. Any other name, short ones included
  (a laptop called `yoga`), is left alone, and bootstrap prints the
  `hostnamectl set-hostname <fqdn>` to run.
- `FLEET_SET_HOSTNAME=1` forces the rename and `FLEET_SET_HOSTNAME=0` skips the
  step. Nothing happens without `hostnamectl`, or when the domain is not a
  dotted FQDN.
- A rename runs `hostnamectl set-hostname <fqdn>`. When `/etc/cloud` exists, it
  writes `/etc/cloud/cloud.cfg.d/99-preserve-hostname.cfg`
  (`preserve_hostname: true`, `manage_etc_hosts: false`) so cloud-init doesn't
  reset the name from Vultr metadata. On the `127.0.0.1`/`::1` lines of
  `/etc/hosts` it replaces the old hostname token with `<fqdn> <short>` and keeps
  every other alias. It never replaces a localhost name. When no line names the
  FQDN, it appends both loopback lines.
- The step is idempotent, honours `--dry-run`, and never fails the bootstrap.

## Deviations

An earlier draft disabled Cockpit (mask the socket, drop it from firewalld,
blank its MOTD notice, and re-apply on every update). Cockpit has since been
uninstalled from every box, so that machinery was dropped in favour of one
bootstrap-time removal.

## Deliberately not done

- `fleet update` neither removes Cockpit nor renames a box. Existing boxes were
  fixed by hand.
- An unset (empty) static hostname is not treated as a default. Only the three
  names above are; use `FLEET_SET_HOSTNAME=1` otherwise.
- `/etc/hosts` is rewritten in place rather than through a temporary file and a
  rename. That keeps the file's inode and SELinux label (`net_conf_t`). The
  trade-off is that a write that dies part-way leaves a truncated file.

## Tests

`internal/admincli/cockpit_lib_test.go` and
`internal/admincli/hostname_lib_test.go` run both functions against stub
`rpm`/`dnf` and `hostnamectl` binaries and scratch `/etc/hosts` and
`/etc/cloud` paths.
