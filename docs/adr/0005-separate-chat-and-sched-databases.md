# ADR-0005: Separate Postgres databases for chat and sched

- **Status:** Accepted
- **Date:** 2026-06-28 (documents a decision that predates this record)
- **Deciders:** fleet maintainers

## Context

fleet has two persistence domains with independently-evolved schemas: the chat
store (conversations, turns, turn-event ledger, full-text search) and the
scheduling engine (task definitions, runs, retries, leases). They were built
with **different migration systems**, and — critically — both name their
bookkeeping table `schema_migrations` with **incompatible** layouts. Point them
at one database and the two migration systems corrupt each other's state.

## Decision

The chat store and the scheduler use **separate Postgres databases**.

- Chat (`internal/store`) migrates via an **advisory-lock** scheme
  (`internal/store/migrations.go` + `internal/store/migrations/*.sql`).
- Sched (`internal/sched/db/migrations`, 30 versions) migrates via
  **golang-migrate**.

Both suites auto-migrate from an empty database. Test and CI configuration must
give them **distinct** DSNs — in CI, `FLEET_TEST_DATABASE_URL` /
`CHAT_TEST_DATABASE_URL` point at one database and `DATABASE_URL` at another
(see `.github/workflows/ci.yml` and `CONTRIBUTING.md`).

## Enforcement

- The two migration directories and systems are physically separate in the tree.
- CI creates two databases and wires the chat and sched suites to different
  DSNs; pointing them at the same database makes the suites fail.

## Consequences

- The two domains can evolve their schemas and migration tooling independently.
- Any cross-domain query is an application-level join across two databases, not
  a SQL join — accepted, because the domains are only loosely related.
- Operators provision and back up two logical databases; the backup/restore
  tooling accounts for both (`docs/BACKUP_RESTORE.md`).
- Local setup and tests **must** use separate DSNs; a single shared DSN is a
  configuration error, not a convenience.

## Alternatives considered

- **One database, one schema.** Rejected: the two `schema_migrations` tables are
  incompatible; unifying them would mean rewriting one domain's entire migration
  history.
- **One database, separate schemas (namespaces).** Considered; still risks the
  two migration tools contending over bookkeeping and offers little benefit over
  separate databases on a single-box Postgres.
