# `fleet timers install` — one-command setup for the scheduled-maintenance timers

fleet ships two systemd service+timer pairs in [`deploy/`](../deploy):

| Pair | What it does | When | Skip it if… |
|---|---|---|---|
| `fleet-backup.{service,timer}` | `fleet backup --db=all --prune` — pg_dump of the chat + sched databases, each dump integrity-checked, old dumps pruned | daily 02:00 | you back up at the volume/hypervisor layer ([docs/BACKUP_RESTORE.md](BACKUP_RESTORE.md)) |
| `fleet-maintenance.{service,timer}` | `fleet cleanup` — prunes dangling podman image layers (~1.3 GB stranded per sandbox rebuild) + build caches | daily 03:30 | you prune the container store yourself ([docs/MAINTENANCE.md](MAINTENANCE.md)) |

`scripts/bootstrap.sh --enable-service` installs both on a new box
(`--no-backup-timer` / `--no-maintenance-timer` opt out). The gap this feature
closes: a box provisioned **before** the timers shipped — or one whose operator
opted out and changed their mind — had no path to them except copy-pasting a
four-command `install`/`daemon-reload`/`enable` hint out of `fleet doctor`.

## The command

```sh
sudo fleet timers install                  # both pairs
sudo fleet timers install --backup         # just the backup pair
sudo fleet timers install --maintenance    # just the maintenance pair
fleet timers install --dry-run             # print the plan; no root needed
```

What one run does, idempotently:

1. Installs the **missing** unit files from the checkout's `deploy/`
   (resolved via `--src`, `FLEET_ROOT`, the working directory, or the
   bootstrap layout's `<install-dir>/src`). A unit that is **already
   installed is never overwritten** — reconciling drift between an installed
   unit and `deploy/` stays `fleet doctor` / `fleet update --adopt-units`
   territory, both of which gate an overwrite on explicit consent, so this
   verb can never clobber an operator hand-edit.
2. For the backup pair, creates the backup directory (0700, root-owned) when
   missing — `FLEET_BACKUP_DIR` from the environment or the server env file,
   else `/var/backups/fleet`. An existing directory keeps whatever mode you
   gave it (it may be a shared mount).
3. `systemctl daemon-reload` (once, only when something was installed), then
   `systemctl enable --now` each selected timer. The enable runs even when
   nothing was installed, so the same command also repairs the two other
   states doctor advises on: installed-but-disabled and enabled-but-stopped.

## Where it is offered

- **`fleet doctor`** (both `scripts/doctor.sh` and the in-process
  `internal/boxdoctor` panel) advises when a pair is missing and its fix hint
  is now this one command. Doctor still never installs the timers itself — an
  operator who declined them is not misconfigured, and doctor overreaching
  past a deliberate `--no-backup-timer` would be worse than the hint.
- **`fleet update`** — after its unit-drift pass, an interactive update
  offers to install a **fully-missing** pair (y/N per pair, default **No**),
  delegating to `fleet timers install` so there is one install
  implementation. Non-interactive runs print the one-liner instead of
  prompting. `--no-timers` (env `FLEET_UPDATE_OFFER_TIMERS=0`) silences both
  for boxes that deliberately run without the timers, so a decline never
  turns into a nag. `update` also now covers the `fleet-maintenance` pair in
  its drift-adoption loop (previously only the backup pair rode it), under
  the same no-restart rule — a reloaded timer re-arms without bouncing the
  app, and the backup oneshot is never restarted (that would run a backup
  immediately).

## Non-systemd deployments (containers, Kubernetes)

These timers are a systemd-deployment concern. On a host without `systemctl`
the command does not pretend: it explains that the equivalent jobs belong to
the platform's scheduler — daily `fleet backup --db=all --prune` and daily
`fleet cleanup` (cron, a Kubernetes CronJob) — and exits non-zero. `fleet
update`'s offer and doctor's advisories are likewise skipped entirely where
there is no systemd. For the first-class Kubernetes deployment, the CronJob
equivalents are part of the production checklist in
[`DEPLOYMENT-KUBERNETES.md`](DEPLOYMENT-KUBERNETES.md).

## Honest scope / deliberately not done

- **No `fleet timers status`.** Timer state is already reported by
  `fleet doctor` and the admin Doctor panel; a third reporter would be one
  more place for the verdicts to drift.
- **No uninstall verb.** `systemctl disable --now <timer>` plus removing the
  two files is the whole operation, and doctor treats an absent pair as an
  advisory, never a failure.
- **The install never overwrites an existing unit file**, even a stale one —
  drift adoption stays consent-gated in `doctor`/`update`.
- **`fleet update`'s offer does not persist a decline** beyond the run;
  silencing it permanently is the explicit `--no-timers` /
  `FLEET_UPDATE_OFFER_TIMERS=0` opt-out (state lives in how you invoke your
  tools, not in a new marker file).
