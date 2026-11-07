# One persistence plane: one database, a schema per store

Issue #2049: phase 1 (milestones M1 + M3, plus the migration tool) and
phase 2 (the legacy two-database layout and the migration tool removed).
Decision record: [ADR-0098](adr/0098-one-database-two-schemas.md), which
supersedes [ADR-0005](adr/0005-separate-chat-and-sched-databases.md).

fleet has two stores: the **chat store** (`internal/store`: conversations,
turns, chat users, …) and the **scheduler** (`internal/sched/db`: tasks, runs,
API keys, orchestrator users, …). They used to need two Postgres databases.
They now live in **one** database, the chat store in schema `chat` and the
scheduler in schema `sched`, configured by one DSN — the only configuration
fleet accepts:

```
DATABASE_URL=postgres://fleet:…@127.0.0.1:5432/fleet?sslmode=disable
```

## What shipped

**Schema mode (M1).** `config.ResolveDSNs` (`internal/config/dsn.go`) is the
one place the DSNs are decided: `DATABASE_URL`, else the `DB_*` parts
(default user and database `fleet` on `localhost:5432`). It gives each
store's DSN its schema as the connections' only `search_path`, through the
libpq/pgx startup option `options=-csearch_path=<schema>`. The edit is in
place — only the `options` query parameter changes — so the shell mirror in
`scripts/lib/idle.sh` (`fleet_resolve_dsn`, used by `update.sh`'s idle gate
to psql into the stores) produces the same bytes;
`TestIdleLibResolveDSNParity` pins the two.

**Where a DSN connects is pgx's answer.** Every question about a DSN's target
(host, port, database, role, password) is answered by `config.ParseConnTarget`,
which is `pgconn.ParseConfig`, the reading the stores' own connections use.
That covers the "same database?" identity (`config.DSNIdentity`, used by the
distinct-stores guard and `fleet status`) and the host, port and role that
`fleet backup`/`restore` hand to pg_dump/pg_restore. Only *rewriting* a DSN
(the schema option) edits the string itself. As in pgx:

- a query parameter (`?host=`, `?port=`, `?dbname=`, `?user=`, `?password=`)
  overrides the URL's own part, and the last occurrence wins;
- what the DSN leaves out comes from the `PG*` environment (`PGHOST`,
  `PGPORT`, `PGDATABASE`, `PGUSER`, a service file), `.pgpass` for the
  password, then the defaults (the default socket directory, port 5432, the OS
  user, and the role name as the database). This is deliberate, because the
  process asking connects with that same environment;
- a DSN listing several hosts (`host=a,b`, `postgres://a,b/db`) has no
  identity and is refused wherever a target is needed: fleet needs one server.

The shell readers of a DSN either follow pgx or refuse: `fleet_dsn_role_db`
(`scripts/lib/pgprovision.sh`) refuses a DSN whose query overrides its role or
database, and bootstrap's `env_dsn_password` takes a `?password=` override.

Because the schema travels inside the DSN, every consumer agrees without
deciding anything: `fleet serve`, the MCP broker, `fleet validate-config`,
`fleet eval`, every admin verb (`status`, `import`, `migrate`, `backup`,
`restore`, `cache-stats`, `diagnose`, the idle checks) and psql/pg_dump.

An admin verb's `--database-url` / `--*-database-url` is resolved by
`DSNs.OverrideFor`: an override that names its own schema
(`?options=-csearch_path%3D<schema>`) is used verbatim; any other override
gets the store's schema, whatever database it names (a DR host, a restored
copy). So a flag can never land a verb in the other store, or in `public`
unless it says so.

Each migration runner first calls `internal/dbschema.Ensure`, which creates
the connection's schema under a transaction advisory lock (Postgres reports
`current_schema()` as NULL until it exists, and `CREATE SCHEMA IF NOT EXISTS`
races). It refuses one shape: a store schema in a database whose `public` schema
holds a fleet store — `DATABASE_URL` pointed at a database of the removed
two-database layout — because empty new schemas would boot an apparently
empty fleet over that data. The check is one catalog query per store open and
stays as a backstop after phase 2. A DSN that names no store schema (the
package test databases until M2) is left in `public`: `Ensure` does nothing.

Verified, not assumed: neither migration set uses an extension or a
schema-qualified name, and `gen_random_uuid` is in `pg_catalog`, which
Postgres searches first whatever the `search_path`.
`TestBothStoresInOneDatabase` (`internal/dbschema`) runs both stores' real
migrations into one scratch database from one DSN, creates a user in each
store's `users` table under the same name, checks every table landed in its
store's schema (nothing in `public`, each `schema_migrations` in its runner's
shape), and reopens both with no migration to run.

`EnsureDistinctDatabases` became `EnsureDistinctStores`: what it refuses is
two DSNs that would put both stores in the **same schema**. `ResolveDSNs`
never produces such a pair; the guard stays as a cheap backstop at boot, in
the broker and `validate-config` (a `Config` assembled some other way), and in
`fleet import`, where two `--*-database-url` flags naming one schema are
refused for a bundle with both sections.

**One DSN (M3).**

- `scripts/bootstrap.sh` provisions one role and one database (default
  `fleet`/`fleet`; `--db-name`, `--db-user`, `--adopt-existing-db`) and writes
  only `DATABASE_URL` (names must be lower-case). External mode reads
  `DATABASE_URL` from the environment, else the env file (read as fleet reads
  it: `KEY=""` is unset), and refuses a database whose `public` holds a fleet
  store, and a role that cannot run the stores: for each of `chat` and
  `sched`, CREATE on the database if the schema does not exist yet, or — if it
  does — OWNERSHIP of the schema and of every table, sequence and view in it
  (the refusal names the owners it found); with
  `FLEET_DB_SUPERUSER_URL` it creates the DSN's database if missing and grants
  the DSN's role CONNECT + CREATE on it.
- The Helm chart's in-cluster Postgres creates one database, `fleet`
  (`POSTGRES_DB`), and the control plane gets `DATABASE_URL`.
- `fleet backup` dumps one schema per store (`pg_dump -n`); `fleet restore`
  reads the dump's table of contents and refuses a dump whose tables are not
  in the target store's schema, before anything is dropped.
- `fleet status` reports the layout of the DSNs it checked;
  `validate-config` names it and runs the same read-only readiness check
  every store open runs (no fleet store in `public`; each store schema
  missing with CREATE on the database, or present and owned by the role with
  everything in it) — the check bootstrap's shell gate mirrors.

## Phase 2: the legacy layout removed

Shipped once every box (all twelve that matter) had been moved into one
database with `fleet db consolidate`, its old `chat` and `sched` databases and
the `chat` role dropped, and its env file reduced to `DATABASE_URL` (the owner
confirmed each). On those boxes the database `fleet` is owned by role `sched`;
nothing in fleet assumes the role's name — it only ever uses `DATABASE_URL`.

**What went:**

- The legacy two-database layout: `resolveLegacyDSNs` and `LayoutOf` /
  `LayoutLegacy` (`internal/config/dsn_legacy.go`), the `Layout` type, the
  boot notice, and the shell mirror `fleet_resolve_dsn_legacy`.
- The per-store keys `FLEET_CHAT_DATABASE_URL`, `FLEET_SCHED_DATABASE_URL` and
  `SCHED_DATABASE_URL`. They are not silently ignored: `config.ResolveDSNs`
  fails with an error naming them and `DATABASE_URL` (so `fleet serve`, the
  MCP broker, `validate-config`, `eval` and every DSN-using CLI verb refuse,
  and `fleet restart`/`stop` refuse to wait on an idle check they cannot
  run), `fleet_resolve_dsn` in `scripts/lib/idle.sh` refuses the same way
  (update.sh's idle gate reports it as blocking work), `fleet doctor` fails
  the box, and `bootstrap.sh` refuses to run. The keys stay in the env-file
  allowlist only so a value there reaches that refusal, and in the broker's
  stripped-env list because they would carry a password.
- `fleet db consolidate`, `fleet db layout` and the hidden
  `fleet db dsn-identity` — the whole `fleet db` verb. Its only remaining
  user, the shell's `fleet_dsn_identity` (and its mirror
  `fleet_dsn_identity_sh`, `fleet_same_database`), served the legacy layout
  decision and went with it. `config.DSNIdentity` stays: the distinct-stores
  guard and `fleet status` use it.
- The `PublicStoreProbe` behind `DSNs.OverrideFor`: an override that names no
  schema now always gets the store's schema.
- Bootstrap's per-store flags (`--chat-db-name`, `--chat-db-user`,
  `--sched-db-name`, `--sched-db-user`, `--adopt-existing-chat-db`,
  `--adopt-existing-sched-db`) and inputs (`CHAT_DB_*`, `SCHED_DB_*`), its
  layout decision and both legacy provisioning branches; `pgprovision.sh`'s
  per-store password placeholders (the redacting runner now takes one
  password).
- update.sh's "consolidate" recommendation and doctor.sh's legacy branch.
- The Helm chart's legacy-release detection (`fleet.postgresLegacyLayout`) and
  its `chat-url` / `sched-url` Secret keys. A release whose in-cluster
  Postgres predates #2049 holds the databases `chat` and `sched` and no
  `fleet`; the chart no longer supports it (reinstall it, or move its data by
  hand as below).
- `fleet backup`'s `-n public` fallback and `fleet restore`'s acceptance of a
  `public` dump.

**Decisions:**

- **The bare `DB_*` defaults** are now one database, user and database
  `fleet`/`fleet`, as bootstrap provisions. (Phase 1 kept the old per-store
  defaults, `chat`/`chat` and `fleet`/`sched`, for a box that might rely on
  them; none does.)
- **Old dumps are refused, not imported.** `fleet restore` of a dump whose
  store is in `public` (one taken before the box moved) refuses with a message
  saying so, before anything is dropped. Every box has fresh schema-mode
  backups; [BACKUP_RESTORE.md](BACKUP_RESTORE.md) documents the four commands
  that import an old dump by hand (restore into a scratch database, rename
  `public` to the store's schema, dump that schema, `fleet restore` it).
- **The `public` guard stays** (`dbschema.Ensure` and bootstrap's
  `fleet_db_store_readiness` refuse a database whose `public` holds a fleet
  store): it costs one catalog query and still catches `DATABASE_URL` pointed
  at an old database. A DSN without a store schema is still left in `public`
  rather than refused, because the package test suites still open their
  stores that way; M2 moves them to schemas, and can then make it a refusal.
- **The admin CLI suites** run in one scratch database with both schemas:
  `internal/admincli`'s `TestMain` creates `<DATABASE_URL's db>_admincli`,
  points `FLEET_TEST_DATABASE_URL` at its `chat` schema and `DATABASE_URL` at
  its `sched` schema, and drops it afterwards (the CI test databases hold
  other packages' stores in `public`, which the stores refuse to run next
  to). M2's template databases replace this.
- `scripts/e2e-boot-server.sh` boots the live e2e suite on one database,
  `fleet_e2e`, with `DATABASE_URL` alone.
- The per-store `--chat-database-url` / `--sched-database-url` flags and
  `ChatFrom`/`SchedFrom` were not merged into one flag: `DSNs` carries one
  `From` now, but the flags stay — each still names the store it addresses,
  and dropping them would break operator scripts for no gain.

**Moving data by hand** (a Helm evaluation release from before #2049, or any
other leftover two-database copy): for each store, restore or copy its
database into a scratch database, `ALTER SCHEMA public RENAME TO <store>`,
`pg_dump -Fc -n <store>`, and `fleet restore --db=<store>` the result into the
one database, as in [BACKUP_RESTORE.md](BACKUP_RESTORE.md).

## What deviated from the issue

- The schema is set through the DSN's `options` startup parameter rather than
  pgx `RuntimeParams`, so psql, pg_dump and the shell helpers get it from the
  same DSN.
- Phase 1 kept the bare `DB_*` defaults on the legacy pair and shipped
  `fleet db consolidate` (URL DSNs on one local server only) and
  `fleet db layout` for the move; phase 2 removed all three (above).
- There was never an in-cluster consolidation for the Helm chart's Postgres
  (an evaluation convenience): such a release is reinstalled or moved by
  hand.

## The rest of #2049 (follow-up PRs, in this order)

Phases 1 and 2 have shipped. The remaining phases ship as their own PRs under
the same issue, each updating this note:

1. **M2 — template test databases:** each test gets a fresh database cloned
   from a migrated template, so the DB test packages run in parallel (and the
   admin CLI's scratch-database `TestMain` goes).
2. **M4 — one identity table:** one `users` table and one role vocabulary;
   the shadow sync and the identity seams go.
3. **M5 — one scheduler:** chat patrols run on the sched engine.
4. **One migration runner** and one migration layout.
