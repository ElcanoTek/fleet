# Keeping the Cockpit web console off

## What shipped

The Vultr Fedora image ships [Cockpit](https://cockpit-project.org/), and its
`cockpit.socket` serves a root-capable web login on port 9090. A fleet box is
administered over ssh only, so that login is a second remote-admin surface,
patched separately, that nothing uses. It was turned off on every box by hand;
the installers now keep it off:

- **`scripts/bootstrap.sh`** turns it off on a new box, right after the system
  dependencies step.
- **`scripts/update.sh`** (what `fleet update` runs) turns it off again on every
  update, before the build. A box provisioned before this change, or one where a
  package update or a hand-run `systemctl enable` brought it back, heals on the
  next update.
- **`scripts/doctor.sh`** checks it in step 6. Under `--check`, a listening
  socket is a failure and an installed-but-unmasked one is an advisory; a plain
  `fleet doctor` applies the same repair.

All three share one implementation, `scripts/lib/cockpit.sh`. It mirrors keel's
`step_cockpit` (`deploy/harden-host.sh`):

1. Only when `systemctl` exists **and** `systemctl list-unit-files
   cockpit.socket` returns a row. Otherwise it prints one info line and does
   nothing else, the firewall half included. That covers containers, hosts
   without systemd, and Debian/Ubuntu without cockpit.
2. When `cockpit.socket` is not already masked: `systemctl disable --now
   cockpit.socket`, then `systemctl mask cockpit.socket`. When it is already
   masked, nothing changes.
3. Masking leaves Cockpit's login-banner notice ("Activate the web console
   with: systemctl enable --now cockpit.socket") in the MOTD: `/etc/motd.d/cockpit`
   links to `/run/cockpit/issue`, which the package's
   `/usr/lib/tmpfiles.d/cockpit-ws.conf` recreates at every boot. When that file
   exists and `systemd-tmpfiles` is available, `deploy/cockpit-ws.tmpfiles.conf`
   is installed as `/etc/tmpfiles.d/cockpit-ws.conf` (the same name overrides the
   package copy and survives cockpit-ws upgrades) and applied with
   `systemd-tmpfiles --create`. It's skipped when the installed copy already
   matches (`cmp`).
4. When `firewall-cmd` exists, `firewall-cmd --state` says `running`, and a
   zone's permanent config lists the `cockpit` service: `firewall-cmd
   --permanent --zone=<zone> --remove-service=cockpit` for each such zone, then
   `firewall-cmd --reload`.

Under `--dry-run` the commands are printed, not run.

## What it does not do

- It never installs or removes a package. The cockpit rpm stays installed; it is
  masked, so nothing (a socket activation, a dependency, an `enable`) can start
  it. `systemctl unmask cockpit.socket` undoes it.
- It never touches ufw or iptables, only firewalld.
- It never fails bootstrap or update. A command that fails (not root, a
  systemctl error) prints a warning with the exact commands to run, and
  `fleet doctor --check` keeps reporting it until it is fixed.
- There is no opt-out flag. A box that wants Cockpit back has to unmask it after
  every update, which is deliberate: fleet boxes are administered over ssh.

## Tests

`internal/admincli/cockpit_lib_test.go` runs `cockpit_keep_off` against stub
`systemctl` and `firewall-cmd` binaries for every case above (no systemd,
cockpit absent, live, already masked, firewalld stopped, dry run, a failing
command, the banner override installed once and then left alone), and pins that both installers call it. The bootstrap and update
dry-run smoke tests assert the step is in each plan.
