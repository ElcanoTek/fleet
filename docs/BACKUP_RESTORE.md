# Backup & restore (disaster recovery)

fleet keeps **two stores in one Postgres database**, each in its own schema
([ONE-DATABASE.md](ONE-DATABASE.md)):

- **chat** (schema `chat`) — conversations, turn events, chat users (the `internal/store` pool).
- **sched** (schema `sched`) — scheduled tasks, API keys, orchestrator users (the `internal/sched/db` pool).

Losing the chat store loses every conversation; losing the sched store loses
every scheduled task. There is no other copy. Back both up.

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
`fleet` binary; the `fleet-admin` spelling was removed with the shim —
[ADR-0060](adr/0060-remove-the-fleet-admin-shim.md).)
Both stores and the identity schema between them live in the one database:
`chat`, `sched`, and `core` (the one users table, #2049 M4 —
[IDENTITY.md](IDENTITY.md)), which both stores' rows reference.

- **`fleet backup` (`--db=all`, the default and the backup timer's)** writes
  **one** file, `fleet-all-<stamp>.dump`: a single `pg_dump -n chat -n core
  -n sched`, so a single snapshot. The accounts and the chat and sched rows
  that name them are consistent with each other.
- **`--db=chat` / `--db=sched`** dump that store's schema **plus `core`**, so
  each per-store dump is self-contained.

`fleet restore --db=all` restores a `fleet-all` dump — all three schemas, in
one transaction. `--db=chat` / `--db=sched` restore one store from any dump
that holds it (a `fleet-all` dump, a per-store dump, or an older two-file
backup's file): chat brings `core` with it (its migrations own it); sched
restores `sched` only and leaves the accounts as they are.
`fleet restore` reads the dump's table of contents first and establishes its
schemas from its SCHEMA and TABLE entries; it refuses a dump that does not
contain the schema the target store uses (the other store's dump, or an old
dump of the removed two-database layout — see below) or whose schemas cannot
be determined. It then restores with `pg_restore -n <schema>`, so only the
store's schema is restored (other schemas in the archive are left out) and
`--clean` can drop only the target store's objects — never the other store's,
which shares the database.

**Dumps from before the move to one database.** A dump taken on a box that
still had two databases (#2049; every box has since moved) holds its store in
schema `public`. `fleet restore` refuses it with a message saying so: restore
a dump taken since the box moved to one database instead. If an old dump is
all there is, import it by hand — restore it into a scratch database, rename
its schema, and dump that schema:

```sh
createdb fleet_import
pg_restore --no-owner --no-acl -d fleet_import fleet-chat-<stamp>.dump
psql -d fleet_import -c 'ALTER SCHEMA public RENAME TO chat'
pg_dump -Fc --no-owner --no-acl -n chat -f fleet-chat-imported.dump fleet_import
fleet restore --db=chat fleet-chat-imported.dump
dropdb fleet_import
```

(the same with `sched` for a sched dump; run it as a role that may create a
database, and with fleet stopped for the restore as below).

## Prerequisites

- The PostgreSQL client tools `pg_dump` and `pg_restore` on `PATH` (same major
  version as the server, or newer).
- The same DSN resolution every other `fleet` operator verb uses: `DATABASE_URL`
  with the store's schema (a `--chat-database-url` / `--sched-database-url`
  gets the store's schema too, unless it names one itself).

  On a bootstrapped box these live in the env file, so the verbs need no extra
  flags when run from the fleet environment.

Connection parameters — **including the password** — are passed to `pg_dump` /
`pg_restore` through their environment (`PGPASSWORD`, `PGHOST`, …), never on the
command line, so a secret never appears in `ps` output. Any DSN printed in a log
line is redacted.

## Back up

```sh
# The whole fleet (chat, core, sched; one snapshot) into the current directory:
fleet backup
# → fleet-all-20260623T140506Z.dump

# One store (plus core), into a chosen directory:
fleet backup --db=chat  --out /var/backups/fleet
fleet backup --db=sched --out /var/backups/fleet
```

`--db` accepts `chat`, `sched`, or `all` (the default). `--db=all` needs the
chat and sched DSNs to reach the same database (they do unless overridden
with `--chat-database-url` / `--sched-database-url`); otherwise it refuses and
asks for one store at a time. The dump filename is
`fleet-<db>-<UTC timestamp>.dump`, so successive backups never clobber one
another. Each dump path is printed on **stdout** (the human-readable progress
line goes to stderr), so a script can capture the paths.

Every dump is **verified** immediately after it is written (`pg_restore --list`
confirms it is a valid custom-format archive); a corrupt dump fails the run
non-zero rather than reporting a false success.

**Output directory** resolves to `--out`, else `FLEET_BACKUP_DIR`, else the
current directory.

**Retention pruning** — `--prune` deletes this tool's own dumps
(`fleet-{all,chat,sched}-*.dump`) older than `FLEET_BACKUP_RETENTION_DAYS` (default
30) from the output directory after a successful backup. The knob must be a
positive whole number: since #1273 it is a row in the one env-knob registry
(`internal/config/knobs.go`), and a malformed or non-positive value **fails the
prune** with an error naming the variable instead of silently pruning against
the 30-day default — deleting backups off a misread retention is not a
recoverable mistake. The backup itself still succeeds; only the prune step
fails, with the usual config-error exit code 1:

```sh
fleet backup --db=all --out /var/backups/fleet --prune
# backed up whole fleet database (chat, core, sched) → …/fleet-all-…dump (verified)
# pruned 3 old backup(s) older than 30 days
```

## Restore

Restore overwrites a live database, so `--db` has no default: you name the
target explicitly.

```sh
# A fleet-all dump: chat, core and sched, in one transaction.
fleet restore --db=all   /var/backups/fleet/fleet-all-20260623T140506Z.dump

# One store (from a fleet-all dump, a per-store dump, or an older two-file backup):
fleet restore --db=chat  /var/backups/fleet/fleet-chat-20260623T140506Z.dump
fleet restore --db=sched /var/backups/fleet/fleet-sched-20260623T140506Z.dump
```

`--db=all` refuses a dump that lacks any of `chat`, `core` and `sched` (a
per-store dump, or a file of an older two-file backup): restore those one
store at a time.

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
fleet restore --db=all fleet-all-….dump
# or, from an older two-file backup:
#   fleet restore --db=chat  fleet-chat-….dump
#   fleet restore --db=sched fleet-sched-….dump
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
and `fleet-web.service`. On an already-provisioned box, install + enable the
pair in one command (it also creates the backup directory; see
[docs/TIMERS.md](TIMERS.md)):

```sh
sudo fleet timers install --backup
```

`fleet update` also offers the install interactively when the pair is missing
(`--no-timers` silences the offer for boxes that back up another way). The
equivalent by hand:

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
`internal/boxdoctor` behind Settings → Admin → System checks) now report:

| State | Verdict |
|---|---|
| the `fleet-backup` timer + service pair not installed (either half missing) | **advisory** — backing up at the volume/hypervisor layer is a valid answer, so this never fails the box, and doctor never installs the timer for you (its hint names the opt-in: `sudo fleet timers install --backup`) |
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
- **It does not capture on-disk files.** `fleet backup` dumps the two stores
  and nothing else — no attachment/upload files, no workspaces (see the file
  list at the top of this page). Restoring the dumps brings back conversations
  and tasks but not the files they reference.
- **It is not tested by taking it.** A dump is verified as a readable archive,
  not as a restorable database. Do the round trip described in
  [Verifying a backup](#verifying-a-backup) at least once.
