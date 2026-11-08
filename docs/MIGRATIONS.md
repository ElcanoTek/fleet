# Database migrations

fleet has **one migration runner, one migration directory and one ledger**
([ADR-0107](adr/0107-one-migration-runner.md), issue #2049 milestone M6). Every
schema change of the one database ([ONE-DATABASE.md](ONE-DATABASE.md)) — the
chat store in schema `chat`, the scheduler in `sched`, and what both read in
`core` — is a version of one sequence, recorded in `core.schema_versions`.

| | |
|---|---|
| Runner | `internal/migrations` (`migrations.Up`) |
| Directory | `internal/migrations/sql/` |
| Ledger | `core.schema_versions (version bigint PK, name, checksum, applied_at, dirty)` |
| Lock | one session advisory lock (`migrations.LockKey`), plus both legacy runners' locks whenever there is work to do |
| CLI | `fleet migrate status [--json]`, `fleet migrate down --to <v>`, `fleet migrate force <v>` |
| HTTP | `GET /admin/migrations` on both servers (admin) |

There is no `fleet migrate up`: **every store open migrates** (`store.Open`,
`scheddb.Init`, and the admin verbs, MCP broker and eval runs that open one).
When the database is current that is one read of the ledger. The scheduler's
open then runs the identity merge (`identity.MergeLegacySchedUsers`, ADR-0105),
the one post-migration step. `scripts/update.sh` and `scripts/bootstrap.sh`
never migrate; the service does on start.

## Layout and numbering

| Files | Versions | Runs with `search_path` |
|---|---|---|
| `sql/legacy/chat/NNN_name.sql` — the chat store's migrations before M6 (forward only) | 1–99 (chat N is version N) | `chat` |
| `sql/legacy/sched/NNN_name.up.sql` + `.down.sql` — the scheduler's migrations before M6 | 101–177 (sched N is version 100+N) | `sched` |
| `sql/NNNN_name.up.sql` + `sql/NNNN_name.down.sql` — every migration since | 200 and up | `pg_catalog` |

The merged order — all of chat, then all of sched, then the new layout — is the
order a fresh install has always run them in. It is load-bearing: sched 077
reads `chat.chat_schedules`, and the identity merge needs `core` (chat 099).

The legacy files are **frozen**: their bytes are what every installed database
ran, and they run exactly as they did, each with its store's schema as the
transaction's `search_path` (`SET LOCAL`). The linter refuses an added or
edited file under `sql/legacy/`.

## Adding a migration

1. Take the next free four-digit version, from `0200`, and add **both halves**:
   `internal/migrations/sql/0200_add_widget_flags.up.sql` and
   `internal/migrations/sql/0200_add_widget_flags.down.sql`. The loader refuses
   a build where either is missing (and the linter refuses the pull request).
2. **Name every object with its schema**: `chat.conversations`,
   `sched.tasks`, `core.users`. A new migration runs with `search_path`
   `pg_catalog`, so an unqualified `CREATE TABLE widgets` fails to resolve — and
   the linter flags it first.
3. The runner wraps each migration (and its ledger row) in one transaction:
   don't write `BEGIN`/`COMMIT`. A migration that fails records nothing and
   leaves nothing behind.
4. The `.down.sql` undoes exactly what the `.up.sql` did, so
   `fleet migrate down` can step back one release.
5. Never rename or edit a shipped migration — add a new one.

The `-- fleet:no-transaction` directive (for `CREATE INDEX CONCURRENTLY` and
the like) is reserved: the loader refuses it today; see ADR-0107's future work.

## How a run goes

`migrations.Up(ctx, db)` takes any pool on the database (the runner names every
schema itself, whatever the pool's `search_path`):

1. **Fast path.** One read of `core.schema_versions`; when it holds every
   version this build ships, nothing dirty and nothing unknown, it returns.
2. Otherwise, on one dedicated connection, it takes the migration lock and both
   legacy locks (the old chat runner's key and golang-migrate's key for schema
   `sched`), so a pre-M6 binary booting at the same time waits instead of
   racing. Each wait is bounded (5 minutes) and logged.
3. `dbschema.Ensure` creates the schemas `chat`, `core` and `sched` that are
   missing, after the one readiness check (the same `fleet validate-config` and
   bootstrap's shell gate run): it refuses a database of the removed
   two-database layout, a role that cannot create a missing schema, or an
   existing schema the role does not own.
4. **Adoption** (below), the first time.
5. It refuses a dirty row and a version this build does not ship (a newer fleet
   migrated the database: no downgrades), then applies every version the ledger
   lacks, ascending, each in its own transaction with its ledger row.

## Adoption

The first start of an M6 release moves the database onto the one ledger, once,
in one transaction under all three locks:

1. If `core.schema_versions` exists, nothing happens.
2. Both legacy ledgers are read — `chat.schema_migrations (version, name,
   applied_at)` and golang-migrate's single row in `sched.schema_migrations
   (version, dirty)` — and checked **before anything changes**. Adoption is
   refused, with an operator message and nothing changed, when the sched row is
   dirty, either ledger holds a version this build does not ship, or the sched
   version is outside 0–77.
3. The ledger is created; chat N is recorded as version N (keeping when it was
   applied) and sched 1..v as 101..100+v.

The legacy ledgers are **left in place**. A box behind the latest legacy release
then applies its backlog as ordinary pending versions, in the merged order;
while it does, the runner also records each legacy version it applies in that
store's legacy ledger (when it exists), so the release before M6 remains a
rollback target until a version ≥ 200 has run. A fresh database takes the same
path: it adopts an empty ledger and grows no legacy ledgers.

**Sentinels.** In the transaction of the first version ≥ 200 the runner writes
`chat.schema_migrations` version 100 `moved_to_core_schema_versions` and sets
`sched.schema_migrations` to 178 (creating either ledger in its old shape when
missing). A pre-M6 binary then refuses the database at boot — its chat runner
refuses a version newer than 99, its sched runner has no migration 178 —
instead of migrating it under a ledger it does not know.

`fleet migrate status`, `fleet status` and `fleet doctor` say whether a database
has adopted; before it has, `status` shows what adoption will record.

### Rolling back M6

Before any version ≥ 200 has run, going back to the release before M6 is: stop
fleet, run this against the one database (as its owner), start the previous
binary. The legacy ledgers are intact (and kept up to date by the runner for a
box that applied a backlog), so the previous release continues from them.
`TestM6RollbackBlock` runs exactly this block.

```sql
-- m6-rollback-begin
DROP TABLE core.schema_versions;
-- m6-rollback-end
```

After a version ≥ 200 has run, first `fleet migrate down --to 177` (which
leaves the sentinels), then remove them and run the block:

```sql
DELETE FROM chat.schema_migrations WHERE version = 100;
UPDATE sched.schema_migrations SET version = 77, dirty = false;
```

A database a fresh M6 release installed has no legacy ledgers, so there is no
release before M6 to go back to.

## Checking status

```sh
fleet migrate status          # human-readable: adopted?, current version, every applied/pending version
fleet migrate status --json   # machine-readable
```

Both are **read-only** (they create nothing, apply nothing) and safe against any
database: never migrated (everything pending), not adopted yet (what adoption
will record, from the legacy ledgers), or ahead of the binary (its unknown
versions are listed under `applied`, so a downgrade is visible).

`--json` emits the one report — `db: "fleet"`, `runner: "fleet"`,
`migration_table: "core.schema_versions"`, `layout: "unified"`, `adopted`,
`current_version`, `dirty`, `applied`, `pending` (each `{version, name,
applied_at}`) — plus, **for one release**, the pre-M6 per-store reports under
`chat` and `sched`, derived from it (legacy numbers and names: `sched` keeps
`current_version` 1–77 and `runner: "golang-migrate"`). Move consumers to the
top-level fields: the next release removes the two keys. When the database
cannot be read, every member carries `error`.

`GET /admin/migrations` on either server (admin) returns the same one report
(not the derived keys); its `db`, `runner`, `migration_table`,
`current_version`, `dirty`, `applied` and `pending` keep their shapes and it adds
`layout: "unified"` ([openapi.yaml](openapi.yaml)).

`fleet status` reports the ledger in a `migrations` line (dirty fails; not
adopted yet or pending is an advisory), `fleet doctor` does the same, and
`fleet diagnose` writes it to `db.txt`.

## A dirty ledger: `fleet migrate force`

Every migration runs in a transaction with its row, so a failed one records
nothing: fix the cause and restart. A `dirty` row only comes from an operator
(or a future non-transactional migration). fleet then refuses to start. Inspect
the database, repair or undo what the migration did, and record the last version
that is really applied:

```sh
fleet migrate force <version>   # asks first; --no-confirm for scripts
```

`force` makes the ledger say "every version up to `<version>` is applied,
cleanly, and nothing above it", running nothing (0: nothing applied). A dirty
**legacy** sched row found at adoption is repaired with the previous release's
`fleet migrate force`, or by hand (`UPDATE sched.schema_migrations SET version =
<last good>, dirty = false`).

## Going down: `fleet migrate down`

```sh
fleet backup --db=all                         # first, always
fleet migrate down --to 177 --dry-run         # what would be undone
fleet migrate down --to 177                   # asks first; --no-confirm for scripts
```

Stop fleet first, then start the release that ships the target version. `down`
runs every applied version above `--to` newest first, each in a transaction
that runs its `.down.sql` and deletes its ledger row, under the migration lock.
It prints the `fleet backup --db=all` advice before anything else. It never goes
below 177: the legacy sched `.down.sql` files stay in the tree as history, and
the legacy chat files never had any.

## Zero-downtime / safe-DDL patterns

| Operation | Safe pattern |
|-----------|-------------|
| **Add a column** | `ADD COLUMN col TYPE` (nullable) or `ADD COLUMN col TYPE NOT NULL DEFAULT <const>`. Never `ADD COLUMN … NOT NULL` **without** a default — on a non-empty table Postgres *rejects* it outright, and it can force a table rewrite under an `ACCESS EXCLUSIVE` lock. For a `NOT NULL` column with no sensible default: add it nullable, backfill, then `SET NOT NULL` in a later migration. |
| **Drop a column** | Destructive (permanent data loss) and, in a rolling deploy, breaks an older binary still reading it. Stop reading the column in app code first; drop it in a follow-on migration. |
| **Rename a column** | Two migrations: (1) add the new column + dual-write in app code, (2) backfill + drop the old column. |
| **Change an enum/type** | `ADD VALUE` a new label; **never** `RENAME VALUE` an existing one — an older binary reading the old label breaks. |
| **Add an index** | See the caveat below. |

### Caveat: `CREATE INDEX CONCURRENTLY` is not usable today

`CREATE INDEX CONCURRENTLY` cannot run inside a transaction block, and every
migration runs in one. For a large table, add the index in a low-traffic window,
or accept the brief lock (the tables here are small on a single-box deploy).
The reserved `-- fleet:no-transaction` directive is the planned way out
(ADR-0107).

## The migration linter

`scripts/check-migrations.sh` (the CI **Migration DDL lint** job, `make lint`,
`make lint-migrations`) checks the files a branch **adds or changes** under
`internal/migrations/sql/` (diffed against the merge-base with `origin/main`, or
`MIGRATION_LINT_BASE`):

- **Layout.** An added or edited file under `sql/legacy/` is refused. A pure
  rename (100% similar) is not a change and is exempt — the M6 move itself was
  one; a rename with edits is checked as a change. A new file must be
  `NNNN_name.up.sql` / `NNNN_name.down.sql` (four digits from `0200`, lower-case
  name) and its other half must exist.
- **Qualified names.** In new files (up and down), an unqualified name after
  `CREATE`/`ALTER`/`DROP TABLE|VIEW|SEQUENCE|TYPE|FUNCTION|INDEX`,
  `CREATE INDEX … ON` / `CREATE TRIGGER … ON`, `EXECUTE FUNCTION`,
  `INSERT INTO`, `UPDATE … SET`, `DELETE FROM`, `TRUNCATE` or `REFERENCES`.
- **Dangerous DDL** in forward (`.up.sql`) files: `ADD COLUMN … NOT NULL`
  without a `DEFAULT` (checked per comma-separated column clause),
  `DROP [COLUMN]`, `ALTER TYPE … RENAME VALUE` — `.down.sql` files are expected
  to drop.

Before matching, SQL comments (`--` and `/* */`) are removed and single-quoted
string literals are blanked, respecting quote state, so a `--` or a keyword
inside a string literal neither hides following DDL nor causes a false hit. (It
is a guardrail, not a full SQL parser: `$$`-quoted bodies and string literals
spanning lines are not tokenized.)

A migration that **must** use a dangerous pattern — most often a deliberate,
reviewed `DROP COLUMN` — opts out of the dangerous-DDL checks with a dedicated
comment **directive line** (the layout and naming checks still apply):

```sql
-- migration-lint: allow-dangerous  drops the never-populated legacy_flags column
```

## Backup before a risky migration

```sh
fleet backup --db=all --out ./backups   # ONE pg_dump -Fc of chat, core and sched: fleet-all-<stamp>.dump
```

and restore it with `fleet restore --db=all <fleet-all-dump>` if needed (one
transaction; [BACKUP_RESTORE.md](BACKUP_RESTORE.md) covers the per-store
restores and which of them are self-contained).

Every dump includes `core`, so `core.schema_versions` travels with it. A
one-store restore (`--db=chat|sched`) of a dump taken since M6 is refused unless
the dump's ledger is at the live version; a dump from before M6 restores and
drops the live ledger, so the next start adopts again.

## Squashing old migrations (documented, not done)

The legacy files (and, later, old new-layout ones) can be squashed into one
baseline — but **only after every box has adopted** (`fleet migrate status`
reports `adopted: true` everywhere, and the release that squashes refuses a
database that has not). The procedure, for a later release:

1. Pick the cut-off version `B` (e.g. 177, or the last version every box
   has).
2. On a fresh database migrated to `B`, `pg_dump --schema-only -n chat -n core
   -n sched` (minus the ledger) gives the baseline. Review it like any
   migration; it must name every object with its schema.
3. Add it as `sql/baseline/NNNN_baseline_through_B.sql`, applied only to a
   database whose ledger is empty, and recorded as every version ≤ `B` (with
   the original names and checksums, kept in a generated manifest), so an
   adopted box sees nothing pending and a fresh one gets the same schema in one
   step.
4. Delete the squashed files, and make `Up` refuse a database whose ledger
   stops below `B` but is not empty (that box must first upgrade through a
   release that still ships them).
5. Prove it with the upgrade test: the schema after "fresh install with the
   baseline" equals "fresh install with the files" (`schemaSnapshot`).

Until then the legacy files stay, frozen.

## Not yet implemented

- **Non-transactional migrations** (`-- fleet:no-transaction`): refused by the
  loader; ADR-0107 lists what they need (a dirty row while running, one
  statement per file, no ledger row in the same transaction).
- **A pre-migration `--backup-first` auto-dump.** Use `fleet backup --db=all`.
