# Backup & restore (disaster recovery)

fleet runs **one Postgres cluster with two logical databases**:

- **chat** — conversations, turn events, chat users (the `internal/store` pool).
- **sched** — scheduled tasks, API keys, orchestrator users (the `internal/sched/db` pool).

Losing the chat DB loses every conversation; losing the sched DB loses every
scheduled task. There is no other copy. Back both up.

The databases are **not** the whole state, though: fleet also persists plain
files on disk that `fleet backup` does **not** capture — attachment/upload
files (`<data dir>/attachments`, whose paths conversation history references
for image replay), task upload files (`<data dir>/temp_uploads`), and the
per-conversation workspaces (`./workspace`). On a systemd deploy these all
live under `/var/lib/fleet` (the unit's `StateDirectory`); a box migrated from
the legacy chat app may additionally serve attachment files from the legacy
data dir at its original path (see [`CUTOVER.md`](CUTOVER.md) step 6). Include
those directories in your file-level backups alongside the DB dumps —
restoring the DBs alone brings back the conversations and tasks but not the
files they reference (history replay silently drops a missing file).

`fleet backup` / `fleet restore` wrap `pg_dump -Fc` (PostgreSQL
custom format) and `pg_restore`. (These are operator-CLI verbs of the unified
`fleet` binary; `fleet-admin backup`/`restore` still works but is deprecated and
will be removed.) Each database is dumped to its **own** file
rather than a single cluster-wide `pg_dumpall`, because the two databases have
independent DSNs (and, in `--postgres=external` deployments, independent
credentials). One file per DB also lets you restore one database without
touching the other.

## Prerequisites

- The PostgreSQL client tools `pg_dump` and `pg_restore` on `PATH` (same major
  version as the server, or newer).
- The same DSN resolution every other `fleet` operator verb uses:
  - chat: `--chat-database-url`, else `FLEET_CHAT_DATABASE_URL`, else `DATABASE_URL`
  - sched: `--sched-database-url`, else `FLEET_SCHED_DATABASE_URL`, else `DATABASE_URL`

  On a bootstrapped box these live in the env file, so the verbs need no extra
  flags when run from the fleet environment.

Connection parameters — **including the password** — are passed to `pg_dump` /
`pg_restore` through their environment (`PGPASSWORD`, `PGHOST`, …), never on the
command line, so a secret never appears in `ps` output. Any DSN printed in a log
line is redacted.

## Back up

```sh
# Both databases into the current directory:
fleet backup
# → fleet-chat-20260623T140506Z.dump
# → fleet-sched-20260623T140506Z.dump

# One database, into a chosen directory:
fleet backup --db=chat  --out /var/backups/fleet
fleet backup --db=sched --out /var/backups/fleet
```

`--db` accepts `chat`, `sched`, or `all` (the default). The dump filename is
`fleet-<db>-<UTC timestamp>.dump`, so successive backups never clobber one
another. Each dump path is printed on **stdout** (the human-readable progress
line goes to stderr), so a script can capture the paths.

Every dump is **verified** immediately after it is written (`pg_restore --list`
confirms it is a valid custom-format archive); a corrupt dump fails the run
non-zero rather than reporting a false success.

**Output directory** resolves to `--out`, else `FLEET_BACKUP_DIR`, else the
current directory.

**Retention pruning** — `--prune` deletes this tool's own dumps
(`fleet-{chat,sched}-*.dump`) older than `FLEET_BACKUP_RETENTION_DAYS` (default
30) from the output directory after a successful backup:

```sh
fleet backup --db=all --out /var/backups/fleet --prune
# backed up chat DB → …/fleet-chat-…dump (verified)
# backed up sched DB → …/fleet-sched-…dump (verified)
# pruned 3 old backup(s) older than 30 days
```

## Restore

Restore is **single-database on purpose** — it overwrites a live database, so you
name the target explicitly; there is no `--db=all`.

```sh
fleet restore --db=chat  /var/backups/fleet/fleet-chat-20260623T140506Z.dump
fleet restore --db=sched /var/backups/fleet/fleet-sched-20260623T140506Z.dump
```

Restore runs `pg_restore --clean --if-exists --no-owner --no-acl`: it drops the
existing objects first, then recreates them from the dump, so it is idempotent
over an already-migrated database and does not fail on role/grant mismatches when
restoring into a differently-owned target (the common cross-box DR case).

Because restore **overwrites a live database**, it first verifies the dump is a
valid archive, then asks for confirmation on an interactive terminal:

```
WARNING: this will OVERWRITE the live chat database from …/fleet-chat-….dump. Continue? [y/N]:
```

Pass `--no-confirm` for scripted restores. In a non-interactive context (a pipe
or CI) without `--no-confirm`, restore refuses rather than silently overwriting.

**Stop the fleet service before restoring** so nothing writes mid-restore:

```sh
fleet stop
fleet restore --db=chat  fleet-chat-….dump
fleet restore --db=sched fleet-sched-….dump
fleet restart
fleet status        # both DBs answer SELECT 1, unit healthy
```

The databases self-migrate on connect, so a dump taken from an older schema is
brought up to date when the service restarts.

## Verifying a backup

A backup you have never restored is not a backup. To verify a dump without
touching production, restore it into a throwaway database:

```sh
createdb fleet_chat_verify
pg_restore --clean --if-exists --no-owner --no-acl -d fleet_chat_verify fleet-chat-….dump
psql -d fleet_chat_verify -c '\dt'     # tables present?
dropdb fleet_chat_verify
```

The automated round-trip test (`internal/admincli/backup_test.go`,
`TestBackupRestoreRoundTrip`) does exactly this against scratch databases: it
seeds a sentinel row, runs the real `pg_dump` wrapper, restores into a fresh
database with the real `pg_restore` wrapper, and asserts the row survives — so
the backup *and* restore paths are exercised, not just the dump.

## Scheduling daily backups

A scheduled backup is a **host** operation — it runs `pg_dump` against the
loopback Postgres and writes to a host directory. It is therefore driven by a
**systemd timer** (or cron), **not** a Fleet scheduled task: a Fleet task runs an
agent inside a network-isolated sandbox that cannot reach the host's Postgres or
filesystem, so it is the wrong mechanism for a host backup.

**`scripts/bootstrap.sh --enable-service` installs and enables the timer by
default.** It creates the backup directory if it is missing (`/var/backups/fleet`,
mode `0700`, root-owned — a dump holds every conversation, task and user row),
writes `FLEET_BACKUP_DIR` and `FLEET_BACKUP_RETENTION_DAYS` into `fleet.env`,
installs the two units, and runs `systemctl enable --now fleet-backup.timer`.
Re-running bootstrap converges rather than duplicating: an already-installed unit
is left alone (unit drift is `fleet doctor`'s job), enabling a live timer is a
no-op, and a backup directory that already exists keeps whatever ownership and
permissions you gave it (bootstrap says so rather than re-moding a shared mount;
the unit's `UMask=0077` keeps the dump *files* owner-only either way). Both
settings resolve **process env > the value already in `fleet.env` > the
default**, so a directory or retention you edited into the env file survives
every later re-run — export `FLEET_BACKUP_DIR` / `FLEET_BACKUP_RETENTION_DAYS`
on the bootstrap command line to change them. `FLEET_BACKUP_DIR` must be
absolute (a relative value would resolve against the unit's `/` working
directory) and the retention a positive integer; bootstrap refuses on the runs
that write those keys.

Pass **`--no-backup-timer`** to opt out — the right choice if you snapshot the
volume or the hypervisor, or ship dumps offsite with your own tooling.

The units live in the repo at [`deploy/fleet-backup.service`](../deploy/fleet-backup.service)
and [`deploy/fleet-backup.timer`](../deploy/fleet-backup.timer), so they are
version-controlled and covered by doctor's unit-drift check like `fleet.service`
and `fleet-web.service`. To install them by hand:

```sh
install -D -m 0644 deploy/fleet-backup.service /etc/systemd/system/fleet-backup.service
install -D -m 0644 deploy/fleet-backup.timer   /etc/systemd/system/fleet-backup.timer
install -d -m 0700 -o root -g root /var/backups/fleet
systemctl daemon-reload && systemctl enable --now fleet-backup.timer
systemctl list-timers fleet-backup.timer   # next + last fire
systemctl start fleet-backup.service       # run one backup now
```

The service reads the same env file as the fleet service, so the DSNs and
`FLEET_BACKUP_DIR` / `FLEET_BACKUP_RETENTION_DAYS` resolve from there; the unit
carries `Environment=FLEET_BACKUP_DIR=/var/backups/fleet` as a fallback default,
which the env file overrides. It runs as **root** — it reads the 0600 root-owned
credential file and writes into the 0700 root-owned backup directory.

The `oneshot` service exits non-zero if any dump fails its integrity check, so a
failed backup surfaces in `systemctl status fleet-backup` and the journal.

## What `fleet doctor` reports

Backups used to be the one operational essential doctor did not check, so a box
with no backups at all could report `38 ok, 0 advisories` (issue #966 — found on
a production deployment five days in). Both halves of doctor
([`scripts/doctor.sh`](../scripts/doctor.sh) and the in-process
`internal/boxdoctor` behind Settings → Admin → Doctor) now report:

| State | Verdict |
|---|---|
| the `fleet-backup` timer + service pair not installed (either half missing) | **advisory** — backing up at the volume/hypervisor layer is a valid answer, so this never fails the box, and doctor never installs the timer for you |
| installed but not enabled | **advisory** — it will never fire; `systemctl enable --now fleet-backup.timer` |
| enabled but not active | **advisory** — `is-enabled` only reads the install symlink, so a stopped timer fires nothing while the service's `Result` still says `success`; `systemctl start fleet-backup.timer` |
| enabled + active, last run failed | **failure** — the dump did not complete; `journalctl -u fleet-backup -n 50` |
| enabled + active, no failed run recorded | ok |

A timer that has been failing for a week is worse than no timer, because the box
looks covered — hence the failed-run case is the one that fails doctor.

## What this does NOT protect against

Be clear-eyed about the scope of a scheduled `fleet backup`:

- **It is not offsite backup.** The dumps land on the same host — usually the
  same disk — as the databases they came from. They survive a bad migration, an
  accidental delete, or a botched restore; they do not survive losing the host,
  the volume, or the datacenter. Copy them somewhere else (object storage,
  another box, a snapshot of the backup volume) if that matters to you, and
  verify the copy by restoring it.
- **It does not capture on-disk files.** `fleet backup` dumps the two databases
  and nothing else — no attachment/upload files, no workspaces (see the file
  list at the top of this page). Restoring the dumps brings back conversations
  and tasks but not the files they reference.
- **It is not tested by taking it.** A dump is verified as a readable archive,
  not as a restorable database. Do the round trip described in
  [Verifying a backup](#verifying-a-backup) at least once.
