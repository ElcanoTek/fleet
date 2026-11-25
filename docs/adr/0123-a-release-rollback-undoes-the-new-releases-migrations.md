# ADR-0123: A release rollback undoes the new release's migrations

- **Status:** Accepted
- **Date:** 2026-11-25
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0100](0100-signed-release-artifacts.md) (decision 6: the
  install's automatic rollback, and `fleet release rollback`, now change the
  database schema), [ADR-0107](0107-one-migration-runner.md) (decision 7: down
  migrations are run unattended, by the rollback, not only by an operator)
- **Design note:** [docs/RELEASE-ARTIFACTS.md](../RELEASE-ARTIFACTS.md#roll-back),
  [docs/MIGRATIONS.md](../MIGRATIONS.md#going-down-fleet-migrate-down)

## Context

ADR-0100 has `fleet release install` restart onto the new binary, wait for
`/readyz`, and roll back to `fleet.prev` when the gate fails. ADR-0107 has
every store open migrate, and a binary refuse a database holding a version it
does not ship ("Refusing to downgrade"). Together they left a gap. A release
that ships a migration applies it on start; when it then fails its gate (a
boot slower than `FLEET_HEALTH_TIMEOUT`, a later boot step failing, a database
that briefly stopped answering), the rollback swapped `fleet.prev` back in and
restarted it, and `fleet.prev` refused the database. systemd restarted it
forever, the rollback reported failure, and the box stayed down until someone
ran `fleet migrate down` with the newer binary. Nothing in the release flow,
`scripts/update.sh` or the docs knew about schema versions, and the docs
presented the rollback as the safety net.

## Decision

1. **The rollback takes the schema back before `fleet.prev` starts.** Before
   it restarts onto the new binary, `fleet release install` reads the versions
   `core.schema_versions` holds and records them beside `fleet.prev`
   (`<install-dir>/fleet.prev.schema`). When the gate fails it stops the
   service, puts `fleet.prev` in place (so a CLI verb opening a store in the
   meantime is refused, not migrated up), reads the versions again (retrying),
   and when versions appeared since — all above the newest recorded one — runs
   `migrations.DownBounded` to that version in-process, then starts
   `fleet.prev`. The installing binary is the new release (update.sh runs it
   so), which ships the down migrations of every version it applied.
2. **When the schema cannot be taken back, the new binary stays.** A version
   read that keeps failing, a down migration that fails (each lock wait is
   bounded, so a DROP behind a `pg_dump` fails with "lock timeout" rather than
   running out the budget), versions that are not a clean tail, an empty
   ledger before, or a restarter that cannot stop the service on its own: the
   new binary goes back (it runs on the database in either state), is
   restarted, and the install exits 7 (ready now) or 10 (still not ready),
   saying it was not rolled back. That includes an install run by hand with
   an older binary, whose Down refuses the versions it does not ship.
3. **The versions are required before an armed install.** With a restart, a
   rollback possible and no `--no-rollback`, an install that cannot read them
   refuses before changing anything (exit 8). Without a rollback armed the
   read is best effort and only feeds the record.
4. **A manual rollback asks.** `fleet release rollback` refuses (exit 9,
   nothing changed) when the database holds migrations above what
   `fleet.prev` ran on, unless `--migrate-down` — by then the newer tables hold
   real data — and never changes the schema with `--no-restart`.
5. **Nothing else changes.** A binary still refuses an unknown schema; the
   service still migrates up on start; `scripts/update.sh` itself runs no
   migration; `helm rollback` runs none either (the chart has no hook).

## Enforcement

- `internal/release/install_schema_test.go`: the refusal loop without a
  Schema, the undo order (stop, swap, down, start), the old-binary-runs-install
  case, every keep-the-new-binary path, the restarts after a failed restore or
  swap, the manual rollback's refusals and swap-back.
- `internal/migrations/runner_test.go` (`TestDownForAnOlderBinary`,
  `TestDownLockTimeout`) and `internal/admincli/release_schema_test.go` (the
  adapter on a real database, the DSN defaults, `--migrate-down`).

## Consequences

- A failed install of a release that migrated comes back up on the previous
  binary, with the data the previous schema holds.
- What the failed release wrote into the tables and columns its migrations
  added is dropped; no backup is taken first. Data an up migration rewrote in
  place (0200's backfill, 0201's `autonomous` → `auto`) is not reverted; the
  previous binary runs on it.
- Every `.down.sql` now runs unattended, so it must undo exactly its `.up.sql`.
- A rollback of a release with no new migration reads the database once more,
  and keeps the new binary if the database cannot be read at all.
- A `fleet.prev` installed by a release before this one has no record; its
  manual rollback only swaps binaries, as before. An older binary running the
  install (by hand) cannot undo newer versions and keeps the new binary.
- Kubernetes is not covered: before `helm rollback` past a migration, an
  operator runs `fleet migrate down` with the newer image.

## Alternatives considered

- **Back up before an install and restore on rollback.** A dump on every
  update is slow on a large database, and a restore loses everything written
  during the window, not only the new objects. Kept as advice.
- **Never roll back past a schema change (keep the new binary).** Leaves a
  genuinely broken release down. It is the fallback when the undo cannot run.
- **Let an older binary run on a newer, additive schema (expand/contract).**
  Reverses ADR-0107's refusal of unknown versions and needs every migration
  classified; out of scope.
