# ADR-0107: One migration runner

- **Status:** Accepted
- **Date:** 2026-11-08
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0098](0098-one-database-two-schemas.md)
- **Issue:** #2049 ("One persistence plane"), milestone M6
- **Design note:** [docs/MIGRATIONS.md](../MIGRATIONS.md)

## Context

Since ADR-0098 both stores live in one database, but each still migrated its
own schema with its own runner: the chat store's hand-rolled runner
(`internal/store/migrations.go`: forward-only `NNN_name.sql`, a set of applied
versions in `chat.schema_migrations`, its own advisory lock) and the
scheduler's in-housed golang-migrate copy (`internal/sched/db/migrate.go`:
`.up`/`.down` pairs, one `{version, dirty}` row in `sched.schema_migrations`,
golang-migrate's lock). Versions 001–077 existed in both, told apart only by
each pool's `search_path`. A fresh install ran all of chat, then all of sched —
an order nothing enforced but that later migrations depend on (sched 077 reads
`chat.chat_schedules`; the identity merge needs `core`). Each runner had its
own lint rules, failure modes (a dirty sched row; a chat migration with no way
back), status report, endpoint and diagnose query, and the test templates were
built twice.

## Decision

1. **One package, one directory.** `internal/migrations` (a leaf: it imports
   neither store) owns the runner and `internal/migrations/sql`. The existing
   files moved there byte for byte: `sql/legacy/chat/` and
   `sql/legacy/sched/`. New migrations are `sql/NNNN_name.up.sql` +
   `.down.sql`, from 0200.
2. **One sequence.** Legacy chat N is version N (1–99), legacy sched N is
   version 100+N (101–177): exactly a fresh install's order. Legacy files run
   with `SET LOCAL search_path` set to their store's schema, as before; new
   files run with `pg_catalog` and name every object with its schema.
3. **One ledger.** `core.schema_versions (version bigint PK, name, checksum,
   applied_at, dirty default false)`. Each migration runs in an explicit
   transaction on the locked connection and inserts its row in it; pending is
   the set difference, applied ascending; a version the build does not ship is
   refused (no downgrades), and so is a row whose name or SHA-256 checksum is
   not the build's file of that version (shipped migrations are append-only;
   legacy rows are verified too — adoption records the build's checksums). The loader refuses a `-- fleet:no-transaction`
   directive for now.
4. **One lock.** A new session advisory key; whenever the runner has work it
   also holds both legacy runners' keys, so a pre-M6 binary booting at the same
   time waits.
5. **One entry point.** `migrations.Up` runs `dbschema.Ensure` for `chat`,
   `core` and `sched` (`core` joins the readiness check and its shell mirror),
   then adoption, then the pending versions; a current database is one read.
   `store.Open` and `scheddb.Init` both call it — any open migrates — and the
   identity merge stays the scheduler's post-migration step.
6. **Adoption.** Once, idempotent, one transaction under all three locks: when
   `core.schema_versions` is missing, read both legacy ledgers; refuse, before
   changing anything, a dirty sched row, a version the build does not ship or
   a sched version outside 0–77; otherwise create the ledger with chat N as N
   (keeping `applied_at`) and sched 1..v as 101..100+v. The legacy ledgers
   stay; a box behind the latest release applies its backlog in the merged
   order. Whenever the ledger crosses 200 — Up applying a version, `force`
   recording one, or a ledger found past 200 without them — the sentinels go
   into the legacy ledgers (chat 100 `moved_to_core_schema_versions`, sched
   178) so a pre-M6 binary refuses the database; the fast path checks them.
7. **Down migrations.** Every version ≥ 200 has both halves (the loader
   refuses a build without). `fleet migrate down --to <v> [--dry-run]
   [--no-confirm]` undoes versions above `v`, newest first, each in a
   transaction that deletes its row, never below 177. `fleet migrate force <v>`
   repairs the one ledger. `fleet migrate status [--json]` reports one stream;
   `--json` keeps `chat` and `sched` as derived views for one release.
8. **Reports.** Both `GET /admin/migrations` keep their JSON keys and add
   `layout: "unified"`; `fleet status`, `fleet doctor` and `fleet diagnose`
   read `core.schema_versions` and report adoption.
9. **Lint.** `scripts/check-migrations.sh` covers `internal/migrations/sql/**`:
   only the exact M6 move into `legacy/` exempt, nothing added, edited,
   renamed or deleted under `legacy/` or in the former directories, no edit,
   rename or deletion of a shipped `sql/NNNN_*` file, a new one numbered after
   the last shipped, new files in
   `NNNN_name` `.up`/`.down` pairs, unqualified names flagged.
10. **Backups.** Every dump carries `core`, ledger included. A one-store
    restore of a post-M6 dump is refused unless its ledger is at the live
    version (use `--db=all`); a pre-M6 dump restored drops the live ledger so
    the next start adopts again.

## Consequences

- One set of rules for every schema change; one place to look at what is
  applied; one test template (`internal/testdb/onedb`, keyed by
  `migrations.Fingerprint`), which the store and scheduler tests now share.
- A failed migration records nothing, legacy sched ones included (they ran
  without an explicit transaction before, but as one implicit one).
- Going back to the release before M6 is `DROP TABLE core.schema_versions`
  while no version ≥ 200 has run (the rollback block in docs/MIGRATIONS.md,
  run by a test). The runner keeps the legacy ledgers in step while it applies
  a legacy backlog, so that holds for a box adopted while behind too
  (a deviation from "leave the legacy ledgers untouched", which still holds for
  adoption itself). After a version ≥ 200, `fleet migrate down --to 177` first.
- A store open on a fresh database creates both stores' schemas, whichever
  store opens first; the "scheduler migrated before chat" order a pre-M6
  release could produce can no longer arise (`EnsureChatScheduleCascade` stays
  for databases that have it).
- Status consumers read one report; the pre-M6 per-store `--json` keys go in
  the next release.
- The in-housed golang-migrate code shrinks to its lock-key formula
  (`LICENSE.golang-migrate` moved with it).

## Future work

- **Non-transactional migrations.** `-- fleet:no-transaction` (for `CREATE
  INDEX CONCURRENTLY`, `ALTER TYPE … ADD VALUE` used in the same release) would
  record the row dirty before running, run one statement outside a
  transaction, then mark it clean — so a failure leaves a dirty row for
  `fleet migrate force`, as golang-migrate did. Refused by the loader until a
  migration needs it.
- **Squashing** the legacy files into a baseline once every box has adopted
  (procedure in docs/MIGRATIONS.md, not executed).
- Removing the derived `chat`/`sched` keys of `fleet migrate status --json`
  and, after a squash, the legacy lock keys.
