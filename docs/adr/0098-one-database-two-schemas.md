# ADR-0098: One database, a schema per store

- **Status:** Accepted
- **Date:** 2026-11-07
- **Deciders:** fleet maintainers
- **Supersedes:** [ADR-0005](0005-separate-chat-and-sched-databases.md)
- **Issue:** #2049 ("One persistence plane"), phase 1 (M1 + M3 + the migration tool)

## Context

ADR-0005 put the chat store and the scheduler in two Postgres databases
because both migration runners name their bookkeeping table
`schema_migrations`, with incompatible layouts, and both stores have a
structurally different `users` table: in one schema they corrupt each other.
It rejected "one database, separate schemas" as offering little over two
databases on a single box.

That cost turned out to be real: every install provisions, configures, backs
up and monitors two databases, two roles and two DSNs; `fleet serve` refused
the one configuration an operator most naturally writes (`DATABASE_URL`
alone); every DSN consumer — the server, the MCP broker, validate-config,
eval, each admin verb, the shell idle gate — had to agree on two precedence
chains. And the later phases of #2049 (template test databases, one identity
table, one scheduler) need both stores reachable from one connection.

Neither original obstacle needs a second database. Both runners already key
their state on `current_schema()` (the sched runner's tracking table and
advisory-lock key are per schema; the chat runner's table is unqualified), no
migration of either store uses an extension or a schema-qualified name, and
`gen_random_uuid` lives in `pg_catalog`, which Postgres searches first
whatever the `search_path`. Verified by running both stores' full migration
sets into one database (`internal/dbschema/onedb_test.go`).

## Decision

**fleet keeps both stores in ONE database, the chat store in schema `chat`
and the scheduler in schema `sched`.** `DATABASE_URL` alone is the
configuration.

- `config.ResolveDSNs` is the one decision point. When the resolved chat and
  sched DSNs name the same database it returns the one-database layout and
  adds each store's schema to its DSN as the connection's only `search_path`
  (the libpq/pgx `options=-csearch_path=<schema>` startup parameter, which
  psql and pg_dump honour too). Every consumer connects with those DSNs and
  so lands in the right schema without deciding anything; the shell mirror
  (`fleet_resolve_dsn`) is pinned to it byte for byte.
- Each migration runner creates its schema before migrating
  (`internal/dbschema.Ensure`, under a transaction advisory lock) and refuses
  to create one in a database whose `public` schema already holds a fleet
  store, so a legacy database is never silently shadowed by empty schemas.
- `EnsureDistinctDatabases` becomes `EnsureDistinctStores`: one database is
  fine; two DSNs that would put both stores in the same schema are refused.
- New installs (bootstrap, the Helm chart's in-cluster Postgres) provision one
  role and one database, `fleet`, and write only `DATABASE_URL`. Backups dump
  one schema per store; restore refuses a dump whose tables are in another
  schema.

**The legacy two-database layout keeps working, unchanged, until each box is
consolidated** (additive-first: an upgrade alone must never break or strand a
box). The per-store keys `FLEET_CHAT_DATABASE_URL`,
`FLEET_SCHED_DATABASE_URL` and `SCHED_DATABASE_URL` are deprecated; with them
— or with the bare per-store `DB_*` defaults — fleet resolves exactly as
before, logs a one-line notice at boot, and `fleet status` / `fleet doctor`
recommend `fleet db consolidate`, an admin verb that moves the two databases
into one (docs/ONE-DATABASE.md). All legacy code carries the marker
`LEGACY-DB-LAYOUT: remove after consolidation` so deleting it is mechanical
once every box has moved.

## Consequences

- One database, one role, one DSN, one backup target per install.
- The two stores still have separate tables, migration runners and `users`
  tables; nothing joins across schemas yet (one identity table and one
  scheduler are later phases of #2049).
- A role that runs fleet needs CREATE on its database (the owner has it) for
  the first boot to create the schemas.
- The chat migration advisory lock is a fixed key, now per database rather
  than per store database; two chat stores in one database (not a supported
  shape) would serialise their migrations, nothing worse.
- Test suites still use separate test databases per suite (their DSNs, CI's
  wiring and `scripts/go-test.sh`'s grouping are unchanged); the regression
  test for this decision runs both stores in one scratch database.

## Alternatives considered

- **Keep two databases (ADR-0005).** Rejected: the operational cost above,
  and it blocks the later phases.
- **One database, one schema** (rename one store's tables and its tracking
  table). Rejected: it rewrites a store's migration history and every query;
  schemas give the same single database for a startup parameter.
- **Auto-consolidate on upgrade.** Rejected: moving production data must be
  an explicit, previewed, verified operator action with a printed rollback,
  not a side effect of `fleet update`.
