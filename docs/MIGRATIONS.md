# Database migrations

Fleet runs its schema migrations automatically at startup (the *auto-migrate*
pattern) across **two separate PostgreSQL databases**, each with its own runner:

| Database | Package | Runner | Files | Down-migrations |
|----------|---------|--------|-------|-----------------|
| **chat** | `internal/store` | hand-rolled (`applyMigrations`) | `NNN_name.sql` | none, by design |
| **sched** (orchestrator) | `internal/sched/db` | [`golang-migrate`](https://github.com/golang-migrate/migrate) | `NNN_name.up.sql` + `NNN_name.down.sql` | yes |

Both are designed for **single-box deployments**: the fleet process migrates its
databases at boot, and a deploy is a brief restart, not a rolling multi-node
rollout. That model shapes the guidance below — some "zero-downtime" rules are
forward-looking (for a future multi-node world), while others prevent a failed or
lock-heavy migration from stalling *this* box's restart.

> `scripts/update.sh` never runs application migrations itself — the services
> self-migrate on restart.

## Chat DB — hand-rolled runner (`internal/store/migrations.go`)

- Migration files are `migrations/NNN_name.sql`, run in ascending numeric order,
  each inside its own transaction, exactly once per database.
- The applied `(version, name, applied_at)` is recorded in `schema_migrations`.
- A `pg_advisory_lock` serializes concurrent boots so two processes can't race
  the same `CREATE TABLE`.
- On failure the transaction rolls back, the DB stays at the last good version,
  and startup fails loudly.
- There are **no down-migrations**. This is a deliberate simplicity choice
  (documented in `migrations.go`): to reverse a bad migration you write a *new*
  forward migration that corrects the state, or restore from a backup. Never edit
  or rename a shipped migration.

To add one:

1. Create `internal/store/migrations/NNN_description.sql` with the next
   zero-padded integer.
2. Write plain DDL. The runner guarantees exactly-once execution.
3. Never touch a shipped migration — add a new one.

## Sched DB — golang-migrate (`internal/sched/db/migrate.go`)

- Migration files are paired `migrations/NNN_name.up.sql` /
  `NNN_name.down.sql`.
- golang-migrate advances the schema **linearly** and tracks a single
  `schema_migrations` row: `{version, dirty}`.
- If a migration fails mid-flight the row is marked **dirty** and the process
  refuses to start until an operator inspects the DB and force-sets the last-good
  version:

  ```sh
  migrate -path internal/sched/db/migrations -database "$FLEET_SCHED_DATABASE_URL" force <version>
  ```

  then restarts fleet.

Every new sched migration must ship both an `.up.sql` and a `.down.sql`.

## Checking migration status

Two read-only, admin-gated endpoints report applied vs pending migrations —
each server reports **its own** database:

- **chat server** — `GET /admin/migrations` → the chat DB status.
- **orchestrator** — `GET /admin/migrations` (documented in `docs/openapi.yaml`)
  → the sched DB status, including the current golang-migrate version and the
  `dirty` flag.

Both apply nothing and are safe to call any time. From the command line, one verb
reports both databases at once:

```sh
fleet migrate status                 # human-readable, both DBs
fleet migrate status --json          # machine-readable, for scripting
fleet migrate status --database-url "$DSN"   # override the DSN for both DBs
```

DSNs resolve the same way as the rest of the CLI: `--database-url`, else
`FLEET_CHAT_DATABASE_URL` / `FLEET_SCHED_DATABASE_URL`, else `DATABASE_URL`. A
database whose DSN is unset is reported as skipped, not an error.

## Zero-downtime / safe-DDL patterns

| Operation | Safe pattern |
|-----------|-------------|
| **Add a column** | `ADD COLUMN col TYPE` (nullable) or `ADD COLUMN col TYPE NOT NULL DEFAULT <const>`. Never `ADD COLUMN … NOT NULL` **without** a default — on a non-empty table Postgres *rejects* it outright, and it can force a table rewrite under an `ACCESS EXCLUSIVE` lock. For a `NOT NULL` column with no sensible default: add it nullable, backfill, then `SET NOT NULL` in a later migration. |
| **Drop a column** | Destructive (permanent data loss) and, in a rolling deploy, breaks an older binary still reading it. Stop reading the column in app code first; drop it in a follow-on migration. |
| **Rename a column** | Two migrations: (1) add the new column + dual-write in app code, (2) backfill + drop the old column. |
| **Change an enum/type** | `ADD VALUE` a new label; **never** `RENAME VALUE` an existing one — an older binary reading the old label breaks. |
| **Add an index** | See the caveat below. |

### Caveat: `CREATE INDEX CONCURRENTLY` is not usable today

The textbook zero-downtime pattern for indexing a large table is
`CREATE INDEX CONCURRENTLY`, which avoids holding a write lock. **Neither runner
supports it right now**, because both wrap each migration in a transaction and
`CREATE INDEX CONCURRENTLY` cannot run inside a transaction block. For a large
table, add the index in a low-traffic window, or accept the brief lock (the
tables here are small on a single-box deploy). A non-transactional migration path
is a possible follow-on.

## The migration DDL linter

`scripts/check-migrations.sh` (wired into CI as the **Migration DDL lint** job
and into `make lint` / `make lint-migrations`) rejects the genuinely dangerous
patterns above in **new or changed** migration files:

1. `ADD COLUMN … NOT NULL` without a `DEFAULT`
2. `DROP COLUMN`
3. `ALTER TYPE … RENAME VALUE`

It is **diff-scoped** — it only inspects migrations added or modified in your
branch (diffed against the merge-base with `origin/main`), so the existing
corpus, which includes several intentional historical `DROP COLUMN`s from this
single-box project, is never re-flagged. `.down.sql` files are skipped (a
rollback is expected to drop). Statements are checked whole, so a pattern split
across lines is still caught, and SQL comments are ignored.

If a migration **must** use a flagged pattern — most often a deliberate,
reviewed `DROP COLUMN` that permanently removes data — opt out explicitly by
adding a marker comment anywhere in the file:

```sql
-- migration-lint: allow-dangerous  drops the never-populated legacy_flags column
```

The justification is visible in review, so the escape hatch stays a conscious,
auditable decision rather than a silent bypass.

## Backup before a risky migration

Before applying a migration you're unsure about, take a dump with the existing
CLI:

```sh
fleet backup --db=all --out ./backups   # pg_dump -Fc, one file per DB
```

and restore with `fleet restore --db=chat|sched <dump-file>` if needed.

## Not yet implemented (honest scope)

- **Automated chat-DB rollback.** The chat runner ships no down-migrations by
  design; a CLI that reverses a hand-rolled migration would reverse that design
  decision and perform a destructive operation, so it is deferred to its own
  reviewed change. Recover today by writing a corrective forward migration or by
  restoring a backup. (The sched DB *does* carry `.down.sql` files; reversing it
  is a manual `migrate … down`/`force` by an operator.)
- **A pre-migration `--backup-first` auto-dump** folded into the migrate path.
  Use `fleet backup` explicitly for now.
