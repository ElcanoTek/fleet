# Bootstrap host hygiene: no Cockpit, a real hostname, key-only SSH

Three small steps `scripts/bootstrap.sh` runs on a new box. `fleet update` runs
none of them.

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

**SSH is key-only.** Vultr's cloud-init writes
`/etc/ssh/sshd_config.d/50-cloud-init.conf` with `PasswordAuthentication yes`,
so boxes took thousands of root password guesses a day. Every box was fixed by
hand, and the Vultr startup script `elcano-key-only-ssh` now applies the fix at
first boot. Bootstrap is the safety net for when nobody selected that script.
`ssh_key_only` (`scripts/lib/ssh.sh`) installs the same file the startup script
writes, `/etc/ssh/sshd_config.d/01-no-password-login.conf`:

```
# Key-only SSH: no passwords, root by key only. Sorts before cloud-init's
# 50-cloud-init.conf so it wins (sshd keeps the first value it reads).
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
```

- `FLEET_KEEP_SSH_PASSWORDS=1` skips it with one info line. The step is silent
  without `sshd` or `/etc/ssh/sshd_config.d`.
- Lockout guard: it counts the authorized keys of root and every `/home/*`
  user, honouring the effective `AuthorizedKeysFile` from `sshd -T` (`%h` and
  `%u`), the way keel's `deploy/harden-host.sh` does. With 0 keys it warns and
  changes nothing.
- When the file already has exactly this content it does nothing. Otherwise it
  writes the file with `install -m 0600`, then runs `sshd -t`. If that fails, it
  removes the file (or puts back the different file that was there) and warns.
  If it passes, it reloads `sshd`, falling back to `ssh` (Debian), and prints
  one ok line. A reload leaves open sessions alone.
- If `sshd -T` still reports `passwordauthentication yes` or
  `permitrootlogin yes` afterwards, an earlier directive wins (for example a
  `PermitRootLogin yes` above the `Include` line in `sshd_config`). It warns and
  leaves `sshd_config` alone.
- It honours `--dry-run` and never fails the bootstrap.

`fleet status` and `fleet doctor` (step 5) warn, never fail, when `sshd -T`
reports `passwordauthentication yes` or `permitrootlogin yes`: "sshd accepts
passwords — see production-support README, Access: SSH keys". Both skip the
check without `sshd`, and when not run as root (`sshd -T` has to read the host
keys).

## Deviations

An earlier draft disabled Cockpit (mask the socket, drop it from firewalld,
blank its MOTD notice, and re-apply on every update). Cockpit has since been
uninstalled from every box, so that machinery was dropped in favour of one
bootstrap-time removal.

## Deliberately not done

- `fleet update` neither removes Cockpit, renames a box, nor touches sshd.
  Existing boxes were fixed by hand.
- Bootstrap never edits `/etc/ssh/sshd_config` itself, and doctor never
  repairs sshd. They only warn.
- An authorized key behind a `command=`/`from=` options prefix, or served by
  `AuthorizedKeysCommand`, is not counted. The guard can only under-count, so
  the worst case is a skipped step with a warning.
- An unset (empty) static hostname is not treated as a default. Only the three
  names above are; use `FLEET_SET_HOSTNAME=1` otherwise.
- `/etc/hosts` is rewritten in place rather than through a temporary file and a
  rename. That keeps the file's inode and SELinux label (`net_conf_t`). The
  trade-off is that a write that dies part-way leaves a truncated file.

## Tests

`internal/admincli/cockpit_lib_test.go`,
`internal/admincli/hostname_lib_test.go` and
`internal/admincli/ssh_lib_test.go` run the functions against stub
`rpm`/`dnf`, `hostnamectl` and `sshd`/`systemctl` binaries and scratch
`/etc/hosts`, `/etc/cloud` and `/etc/ssh` paths. `ssh_lib_test.go` also pins
the `sshd -T` parsing behind the `fleet status` warning.
