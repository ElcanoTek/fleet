# One persistence plane: one database, a schema per store

Issue #2049, phase 1 (milestones M1 + M3, plus the migration tool).
Decision record: [ADR-0098](adr/0098-one-database-two-schemas.md), which
supersedes [ADR-0005](adr/0005-separate-chat-and-sched-databases.md).

fleet has two stores: the **chat store** (`internal/store`: conversations,
turns, chat users, …) and the **scheduler** (`internal/sched/db`: tasks, runs,
API keys, orchestrator users, …). They used to need two Postgres databases.
They now live in **one** database, the chat store in schema `chat` and the
scheduler in schema `sched`, configured by one DSN:

```
DATABASE_URL=postgres://fleet:…@127.0.0.1:5432/fleet?sslmode=disable
```

## What shipped

**Schema mode (M1).** `config.ResolveDSNs` (`internal/config/dsn.go`) is the
one place the layout is decided. When the resolved chat and sched DSNs name the
same database (`host:port/dbname`, or identical DSNs) it returns
`Layout: schemas` and gives each store's DSN its schema as the connections'
only `search_path`, through the libpq/pgx startup option
`options=-csearch_path=<schema>`. The edit is in place — only the `options`
query parameter changes — so the shell mirror in `scripts/lib/idle.sh`
(`fleet_resolve_dsn`, used by `update.sh`'s idle gate to psql into the stores)
produces the same bytes; `TestIdleLibResolveDSNParity` pins the two.

Because the schema travels inside the DSN, every consumer agrees without
deciding anything: `fleet serve`, the MCP broker, `fleet validate-config`,
`fleet eval`, every admin verb (`status`, `import`, `migrate`, `backup`,
`restore`, `cache-stats`, `diagnose`, the idle checks) and psql/pg_dump.

An admin verb's `--database-url` / `--*-database-url` gets its layout from
`DSNs.OverrideFor`, the one place an override is decided:

- an override that names its own schema is used verbatim
  (`?options=-csearch_path%3Dpublic` addresses a legacy store explicitly);
- an override naming the deployment's own database follows the deployment's
  layout;
- an override naming **any other** database — a DR host with no env, a
  restored copy — is decided by its data: the CLI probes it read-only for a
  fleet store in `public` (`public.schema_migrations`). A legacy store is
  used verbatim; anything else (a one-database install, or an empty database
  being restored into) gets the store's schema, because the one-database
  layout is the only one fleet provisions. If the probe cannot connect, the
  deployment's layout decides (the verb then fails to connect anyway).

So a flag can never land a verb in the other store, or in `public` of a
one-database install.

Each migration runner first calls `internal/dbschema.Ensure`, which creates
the connection's schema under a transaction advisory lock (Postgres reports
`current_schema()` as NULL until it exists, and `CREATE SCHEMA IF NOT EXISTS`
races). It refuses one shape: creating a store schema in a database whose
`public` schema already holds a fleet store — i.e. `DATABASE_URL` pointed at
one database of a legacy pair — because empty new schemas would boot an
apparently empty fleet over the real data.

Verified, not assumed: neither migration set uses an extension or a
schema-qualified name, and `gen_random_uuid` is in `pg_catalog`, which
Postgres searches first whatever the `search_path`.
`TestBothStoresInOneDatabase` (`internal/dbschema`) runs both stores' real
migrations into one scratch database from one DSN, creates a user in each
store's `users` table under the same name, checks every table landed in its
store's schema (nothing in `public`, each `schema_migrations` in its runner's
shape), and reopens both with no migration to run.

`EnsureDistinctDatabases` became `EnsureDistinctStores`: one database is the
supported layout; what it still refuses is two DSNs that would put both
stores in the **same schema** (two hand-written DSNs to one database with no
schema, or one schema forced on both).

**One DSN (M3).**

- `scripts/bootstrap.sh` provisions one role and one database (default
  `fleet`/`fleet`; `--db-name`, `--db-user`, `--adopt-existing-db`) and writes
  only `DATABASE_URL`. External mode validates `DATABASE_URL`.
- The Helm chart's in-cluster Postgres creates one database, `fleet`
  (`POSTGRES_DB`), and the control plane gets `DATABASE_URL`.
- `fleet backup` dumps one schema per store (`pg_dump -n`); `fleet restore`
  reads the dump's table of contents and refuses a dump whose tables are not
  in the target store's schema, before anything is dropped.
- `fleet serve` logs a one-line notice on a legacy box; `fleet status` and
  `fleet doctor` report the layout and recommend consolidation;
  `validate-config` names the layout; `fleet db layout` prints it.

**`fleet db consolidate`** moves a legacy box into one database (below).

## The legacy layout (deprecated, still supported)

Every install bootstrapped before #2049 has two databases (`chat`, `sched`)
and the per-store keys `FLEET_CHAT_DATABASE_URL` / `FLEET_SCHED_DATABASE_URL`
(or `SCHED_DATABASE_URL`). Those keys are deprecated. Additive-first: an
upgrade alone never breaks or moves such a box —

- the server resolves it exactly as before (`resolveLegacyDSNs`,
  `internal/config/dsn_legacy.go`), both stores in their databases' `public`;
- `bootstrap.sh` re-runs converge the two databases as they are;
- `update.sh` never consolidates; after an update it prints the
  recommendation;
- a Helm release installed before #2049 (its Postgres Secret carries
  `chat-url`/`sched-url`) keeps its two databases and per-store env on upgrade.

When the per-store keys name the same database, fleet uses that database's two
schemas.

### The `DB_*` parts defaults: why the bare defaults did not change

With no DSN configured, the `DB_*` parts used to default to user/database
`chat`/`chat` for chat and `fleet`/`sched` for sched — two databases. Changing
that default to one `fleet` database would silently point any box that relied
on the bare defaults (a dev box with `chat` and `sched` databases and nothing
configured; no bootstrapped box, which always writes explicit DSNs, but we
cannot prove there is no such box) at a different, empty database. So:

- explicit configuration resolves exactly as before;
- the **bare** defaults (no `DATABASE_URL`, no `DB_NAME`) stay the legacy
  pair, in `dsn_legacy.go`;
- `DB_NAME` set means one database, and `DB_USER` then defaults to `fleet` for
  both stores (that shape was refused at boot before, so nothing relied on it).

When the legacy code is removed the bare default becomes `fleet`/`fleet`.

## Moving a legacy install: `fleet db consolidate`

Run it as **root on the box that hosts Postgres**, with fleet stopped. It
always starts with a dry run.

```sh
# 1. Preview: sources, target, every preflight verdict, exact per-table row
#    counts and every step. Changes nothing.
sudo fleet db consolidate

# 2. Stop everything that touches the databases.
sudo systemctl stop fleet fleet-backup.timer fleet-maintenance.timer

# 3. Move.
sudo fleet db consolidate --apply

# 4. Start again and check.
sudo systemctl start fleet fleet-backup.timer fleet-maintenance.timer
fleet status            # "database layout: one database, schemas chat + sched"
fleet backup            # a first backup of the new layout
```

Flags: `--target-db <name>` (default `fleet`), `--env-file <path>` (default:
the server env file), `--force-clean` (see re-runs).

**What it does.** The target database is created owned by the **sched DSN's
role** and reached with that role's password — no new secret. For each store:
`CREATE DATABASE <target>_consolidate_<store> TEMPLATE <source>` (a scratch
copy; the source is never modified), `ALTER SCHEMA public RENAME TO <store>`
in the copy, `pg_dump -Fc -n <store> | pg_restore --single-transaction
--no-owner --no-privileges --role=<owner> -d <target>`, then the scratch copy
is dropped — always, also on a failure or an interrupt. Superuser work runs as
`runuser -u postgres -- psql/pg_dump/pg_restore` (peer authentication, no
password anywhere), aimed at the instance the legacy DSNs name: their socket
directory when they give one (`?host=/dir`), else the default socket
directory (which loopback TCP DSNs map to), and their port. Passwords are
scrubbed from tool output where they appear as a credential (a DSN's
userinfo, a `password=` pair), and anywhere else only when 12+ characters
long — database and role names are never rewritten.

**Preflight — any failure refuses, and changes nothing:** `fleet.service` and
the backup/maintenance services and timers inactive; no client connection
(`backend_type = 'client backend'`; autovacuum and parallel workers do not
count; a busy answer is re-checked for up to 15 s, since a just-closed pool can
linger) to
either source or the target (`pg_stat_activity`); `psql`, `pg_dump`,
`pg_restore`, `runuser` present and `pg_dump` not older than the server; both
sources exist, are fleet stores (`public.schema_migrations`) and are not
already in schema mode; the sched migrations are not dirty; the target role
exists; the target is absent, or exists owned by that role with empty
`chat`/`sched` schemas and nothing in `public`; enough free disk at the data
directory (about twice the sources' size, best effort); both sources on the
same local instance (same socket directory or loopback, same port) and the
server that answers the superuser connection on that port; URL DSNs. It also
refuses a box whose env file already
configures one database ("Nothing to do").

**Verification — before the env file is touched:** exact per-table row
counts, sequence values and `schema_migrations` rows, target versus the
inventory taken before the copy; the sources recounted to prove nothing wrote
to them meanwhile; every schema, relation, type and function in `chat`/`sched`
owned by the target role; `public` of the target empty; then both stores
opened through fleet's own code against the target in schema mode — no
migration may be pending — and read from.

**The env file** is backed up to `<env>.pre-consolidate-<UTC timestamp>`
(0600, same owner), then rewritten in one atomic write: `DATABASE_URL` set to
the target, and `FLEET_CHAT_DATABASE_URL`, `FLEET_SCHED_DATABASE_URL`,
`SCHED_DATABASE_URL` (and the `DB_*` parts, when they were the source)
commented out with a dated note. The result is re-read; if it does not resolve
to the target in schema mode the backup is restored.

**Interrupting and re-running.** Interrupt it anywhere: until verification
passes the env file is untouched and fleet still runs on the legacy layout.
Each restore is one transaction, so a store is either fully copied or not at
all. A re-run drops leftover scratch databases. A target that already has
`chat`/`sched` schemas (a run that stopped after the first store) is refused;
`--force-clean` drops those two schemas — only those, only in the target,
and only while the env file still names the legacy sources (anything else
stops at "Nothing to do"). The tool never drops a source.

**Rollback** (printed with the exact paths): stop fleet, copy the env-file
backup back, start fleet. The old databases were never modified, so this
returns the box to its state before the move; anything written after the
switch exists only in the target and is not carried back.

**Afterwards**, once satisfied: drop the old databases
(`runuser -u postgres -- psql -c 'DROP DATABASE "chat"' -c 'DROP DATABASE "sched"'`),
the chat role if nothing else uses it, and the commented-out keys.

Tests: `internal/admincli/db_consolidate_test.go` drives the plan, every
preflight refusal and every verification failure through a scripted fake
superuser runner (the env file must stay byte-identical on every failure);
`db_consolidate_e2e_test.go` consolidates two real databases filled by the
real migrations with the real client tools, including a refused and then
`--force-clean`ed half-filled target, a sequence value, ownership, untouched
sources and an idempotent re-run; `TestConsolidateEndToEndRunuser` runs the
same move through the production runner (`runuser -u postgres`, peer auth)
when the test runs as root with a local cluster.

## What deviated from the issue

- The schema is set through the DSN's `options` startup parameter rather than
  pgx `RuntimeParams`, so psql, pg_dump and the shell helpers get it from the
  same DSN.
- The bare `DB_*` defaults were deliberately left as the legacy pair (above).
- Consolidation supports URL DSNs on one local server only; key=value DSNs
  and remote servers are refused with a message (the on-box installs
  bootstrap writes are URLs on loopback).
- `fleet db layout` was added for `update.sh` / `doctor.sh`.
- Helm keeps a pre-#2049 release on its two databases. There is no in-cluster
  consolidation: `fleet db consolidate` needs `runuser -u postgres` on the
  Postgres host. The chart's Postgres is an evaluation convenience; moving
  such a release is a manual dump/restore (not covered here) or a reinstall.

## The rest of #2049 (follow-up PRs, in this order)

This PR is phase 1. The remaining phases ship as their own PRs under the same
issue, each updating this note:

1. **Removing the legacy layout** (below), once every box is consolidated.
2. **M2 — template test databases:** each test gets a fresh database cloned
   from a migrated template, so the DB test packages run in parallel.
3. **M4 — one identity table:** one `users` table and one role vocabulary;
   the shadow sync and the identity seams go.
4. **M5 — one scheduler:** chat patrols run on the sched engine.
5. **One migration runner** and one migration layout.

## Removing the legacy layout

Once every box reports `fleet db layout` → `schemas`:

1. `git grep -n LEGACY-DB-LAYOUT` — every touch point carries this marker
   (`# LEGACY-DB-LAYOUT` in shell, Helm and YAML).
2. Delete whole files: `internal/config/dsn_legacy.go`,
   `internal/config/dsn_legacy_test.go`,
   `internal/admincli/db_consolidate.go`, `db_consolidate_linux.go`,
   `db_consolidate_other.go`, `db_consolidate_test.go`,
   `db_consolidate_e2e_test.go` (the tool goes with the layout, or stays only
   while it is useful), and `internal/admincli/main_test_env_test.go` — the
   `TestMain` that pins the admin CLI suites to the CI test databases'
   legacy layout (`FLEET_TEST_DATABASE_URL` = chat, `DATABASE_URL` = sched,
   both in `public`). Removing it means moving those suites to one test
   database with schemas, which is M2's job.
3. Delete the marked lines/branches: the `resolveLegacyDSNs` call in
   `ResolveDSNs`; `LayoutOf`/`LayoutLegacy` uses in `cmd/fleet/main.go`,
   `validate_config.go`, `internal/admincli/status.go`; the `db` verb and its
   usage lines in `internal/admincli/main.go`; the `public` fallback in
   `backup.go`'s restore check; the `public` no-op in
   `internal/dbschema.Configured` (decide whether a store without a schema
   should then refuse); `fleet_resolve_dsn_legacy` in `scripts/lib/idle.sh`
   and the legacy rows of `idle_dsn_parity_test.go`; the legacy flags,
   inputs and branches in `scripts/bootstrap.sh` and the per-store
   placeholders in `scripts/lib/pgprovision.sh` (and the legacy bootstrap
   tests in `scripts_dryrun_test.go` that pass `--chat-db-*` or per-store env
   files, plus `TestBootstrapKeepsALegacyInstallLegacy`); the recommendation
   in `scripts/update.sh`; the per-store branch in `scripts/doctor.sh`; the
   `fleet.postgresLegacyLayout` helper and its two uses in the Helm chart.
4. Drop the keys: `FLEET_CHAT_DATABASE_URL`, `FLEET_SCHED_DATABASE_URL`,
   `SCHED_DATABASE_URL` from `allowedEnvVars` (`internal/config/config.go`)
   and the broker's stripped-env list (`cmd/fleet/mcp_broker_runtime.go`), and
   from docs (`OPERATORS.md`, `BACKUP_RESTORE.md`, `MIGRATIONS.md`,
   `CUTOVER.md`, `LEGACY-IMPORT.md`). The bare `DB_*` default becomes
   `fleet`/`fleet` (already the non-legacy path in `ResolveDSNs`).
5. Consider replacing the per-store `ChatFrom`/`SchedFrom` and the
   `--chat-database-url`/`--sched-database-url` flags with one source and one
   flag.
