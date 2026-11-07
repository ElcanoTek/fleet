# ADR-0099: The CHAT_/CUTLASS_ env aliases are deprecated, migrated, then removed

- **Status:** Accepted
- **Date:** 2026-11-07
- **Deciders:** Fleet maintainers (owner direction, #2053)
- **Relates to:** [ADR-0012](0012-unified-fleet-cli.md) (one `fleet` binary),
  [`docs/CONFIG-REGISTRY.md`](../CONFIG-REGISTRY.md)

## Context

fleet is the union of two older front-ends, chat-server and cutlass, which
spelled their settings `CHAT_<X>` and `CUTLASS_<X>`. The unified binary reads
the canonical `FLEET_<X>` and, for back-compat, falls back to `CHAT_<X>` and
then `CUTLASS_<X>` (config's `lookupFleet`, agentcore's `fleetEnv`). That chain
covers about 150 settings, so roughly 300 alias spellings are live.

They cost more than they save:

- The same bug kept coming back. Which spellings the env file admitted was a
  hand-kept list, separate from which spellings the loader resolved, and the
  two drifted at least three times (#133, #1015, #2018): an alias that worked
  from the process environment was silently dropped from the env file, or a
  canonical name was never read at all.
- Of the ~300 spellings, only 22 have ever been admitted from the env file;
  the rest work only when set in the process environment. Nobody can tell
  which from the docs.
- Every reader of a setting has to remember the chain. Some readers outside
  `internal/config` honoured `CHAT_` but not `CUTLASS_`, or neither.

The owner controls every box fleet runs on, so there is no external
deployment whose env file must keep working indefinitely.

## Decision

1. **The legacy `CHAT_<X>` / `CUTLASS_<X>` spellings of a `FLEET_<X>` setting
   are deprecated.** They keep working, exactly as before, until they are
   removed. The configuration registry (`internal/config/envreg`) marks each
   one `Deprecated`; `docs/CONFIGURATION.md` lists them all, with whether the
   env file admits each.
2. **`fleet doctor` warns** about every deprecated spelling it finds set in
   the process environment or the server env file, names the canonical key,
   and points at the rewrite. It never prints a value.
3. **`fleet config migrate-env`** rewrites the env file to the canonical keys.
   It is a dry run by default. `--write` copies the file to
   `<file>.bak-<UTC timestamp>` first, then rewrites it atomically (0600,
   owner kept), changing only the key token of each line, so comments, order
   and values are preserved. It refuses when two spellings of one setting
   (or two lines of one alias) disagree and never prints a value. It
   rewrites an alias fleet's own env-file loader does not admit as well:
   the standard install's systemd unit exports every line of the file
   (`EnvironmentFile=`), so such a line is effective there and must keep
   working after removal. The one exception is a *reader-specific* alias
   (the `CHAT_`/`CUTLASS_TLS_*` spellings, read only by the CLI's local
   health probe), which the server ignores. It is left in place and
   reported, because renaming it would start applying it to the server.
   `fleet config explain` and `fleet doctor` count systemd's export the same
   way.
4. **Removal is a follow-up PR, not a dated window.** Once the owner has run
   `fleet config migrate-env --write` on every box, that PR deletes the alias
   resolution, the doctor warning and migrate-env, following the checklist in
   [`docs/CONFIG-REGISTRY.md`](../CONFIG-REGISTRY.md#removing-the-aliases).
   Every code touch point carries the marker
   `LEGACY-ENV-ALIAS: remove after migrate-env`. There is deliberately no
   multi-release timeline (VERSIONING.md's dated windows are for deprecations
   with consumers fleet does not control).

**Not in scope.** Several `CHAT_`/`CUTLASS_` names are canonical settings,
not aliases, and are not deprecated: `CHAT_RATE_PER_MIN`,
`CHAT_RATE_PER_DAY`, the connector wire keys `CUTLASS_ALLOWED_DIRS`,
`CUTLASS_IMAGE_OUTPUT`, `CUTLASS_IMAGE_MODEL`, and the script inputs
`CHAT_DB_*`, `CHAT_URL`, `CHAT_PORT`, `CHAT_DSN`. The bare `PERSONA` /
`PERSONA_DEFAULT` spellings are alternate spellings, not deprecated ones.

## Enforcement

- `internal/config/envreg/table.go`: `Chain` entries derive their deprecated
  aliases; `TestDeprecatedAliasesAreOnlyLegacyPrefixed` keeps "deprecated" to
  exactly the CHAT_/CUTLASS_ spelling of a FLEET_ name.
- `TestAllowlistMatchesPreRegistrySnapshot` and
  `TestKnobsMatchPreRegistrySnapshot` (`internal/config`) pin that the aliases
  still resolve and are admitted exactly as before.
- `TestPlanEnvMigrationRewritesKeysOnly`,
  `TestPlanEnvMigrationRefusesConflictingSpellings`,
  `TestConfigMigrateEnvDryRunThenWrite` and
  `TestDoctorWarnsAboutDeprecatedAliases` pin the migration and the warning.

## Consequences

- Operators see, once per `fleet doctor`, every legacy spelling still in use,
  and have a safe one-command rewrite.
- Until the follow-up lands, the alias machinery stays, isolated behind the
  marker; the follow-up shrinks the registry's alias fields and the loader.
- After removal, a box that skipped the migration silently falls back to
  defaults for any setting it only spelled the old way. That is why removal
  waits for the owner's confirmation rather than a date.

## Alternatives considered

- **A dated deprecation window** (VERSIONING.md style). Rejected: every box
  is owner-controlled, so the removal can follow the migration directly
  instead of waiting out a calendar.
- **Remove the aliases now.** Rejected: boxes would lose settings on upgrade
  before anyone had rewritten their env files.
- **Rewrite env files automatically during `fleet update`.** Rejected: a
  rewrite of the credentials file should be an explicit, reviewable operator
  action with a backup, not a side effect of an update.
