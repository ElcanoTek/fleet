# Backup & restore (disaster recovery)

fleet runs **one Postgres cluster with two logical databases**:

- **chat** — conversations, turn events, chat users (the `internal/store` pool).
- **sched** — scheduled tasks, nodes, API keys (the `internal/sched/db` pool).

Losing the chat DB loses every conversation; losing the sched DB loses every
scheduled task. There is no other copy. Back both up.

`fleet-admin backup` / `fleet-admin restore` wrap `pg_dump -Fc` (PostgreSQL
custom format) and `pg_restore`. Each database is dumped to its **own** file
rather than a single cluster-wide `pg_dumpall`, because the two databases have
independent DSNs (and, in `--postgres=external` deployments, independent
credentials). One file per DB also lets you restore one database without
touching the other.

## Prerequisites

- The PostgreSQL client tools `pg_dump` and `pg_restore` on `PATH` (same major
  version as the server, or newer).
- The same DSN resolution every other `fleet-admin` verb uses:
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
fleet-admin backup
# → fleet-chat-20260623T140506Z.dump
# → fleet-sched-20260623T140506Z.dump

# One database, into a chosen directory:
fleet-admin backup --db=chat  --out /var/backups/fleet
fleet-admin backup --db=sched --out /var/backups/fleet
```

`--db` accepts `chat`, `sched`, or `all` (the default). The dump filename is
`fleet-<db>-<UTC timestamp>.dump`, so successive backups never clobber one
another. Each dump path is printed on **stdout** (the human-readable progress
line goes to stderr), so a cron job can capture the paths:

```sh
# /etc/cron.daily/fleet-backup  (illustrative)
set -euo pipefail
out=/var/backups/fleet
fleet-admin backup --db=all --out "$out" >/dev/null
# prune dumps older than 30 days
find "$out" -name 'fleet-*.dump' -mtime +30 -delete
```

## Restore

Restore is **single-database on purpose** — it overwrites a live database, so you
name the target explicitly; there is no `--db=all`.

```sh
fleet-admin restore --db=chat  /var/backups/fleet/fleet-chat-20260623T140506Z.dump
fleet-admin restore --db=sched /var/backups/fleet/fleet-sched-20260623T140506Z.dump
```

Restore runs `pg_restore --clean --if-exists --no-owner --no-acl`: it drops the
existing objects first, then recreates them from the dump, so it is idempotent
over an already-migrated database and does not fail on role/grant mismatches when
restoring into a differently-owned target (the common cross-box DR case).

**Stop the fleet service before restoring** so nothing writes mid-restore:

```sh
fleet-admin stop
fleet-admin restore --db=chat  fleet-chat-….dump
fleet-admin restore --db=sched fleet-sched-….dump
fleet-admin restart
fleet-admin status        # both DBs answer SELECT 1, unit healthy
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

The automated round-trip test (`cmd/fleet-admin/backup_test.go`,
`TestBackupRestoreRoundTrip`) does exactly this against scratch databases: it
seeds a sentinel row, runs the real `pg_dump` wrapper, restores into a fresh
database with the real `pg_restore` wrapper, and asserts the row survives — so
the backup *and* restore paths are exercised, not just the dump.
