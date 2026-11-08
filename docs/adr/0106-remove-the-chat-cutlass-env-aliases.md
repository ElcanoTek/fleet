# ADR-0106: The CHAT_/CUTLASS_ env aliases are removed

- **Status:** Accepted
- **Date:** 2026-11-07
- **Deciders:** Fleet maintainers (owner direction, #2057 follow-up)
- **Supersedes:** [ADR-0099](0099-deprecate-the-chat-cutlass-env-aliases.md)
  (its "they keep working until removed" part; its distinction between
  aliases and canonical `CHAT_`/`CUTLASS_` names stands)
- **Relates to:** [`docs/CONFIG-REGISTRY.md`](../CONFIG-REGISTRY.md)

## Context

ADR-0099 deprecated the legacy `CHAT_<X>` / `CUTLASS_<X>` spellings of every
`FLEET_<X>` setting, added `fleet doctor` warnings and `fleet config
migrate-env`, and left the removal to a follow-up PR once every
owner-controlled box was migrated. The owner has confirmed that it is: on all
12 boxes `fleet config migrate-env` reported "nothing to rewrite", and `fleet
config explain` and `fleet doctor` are clean. So the removal needs no
compatibility window.

The env files were migrated; client bundles (owned in the client config
repos) are not something migrate-env touched. The contract below is what keeps
an upgrade from taking a box down over a bundle that still uses an old
spelling: it is refused before install, not tolerated at run time.

## Decision

1. **Nothing reads a `CHAT_<X>` / `CUTLASS_<X>` spelling of a `FLEET_<X>`
   setting any more.** The registry (`internal/config/envreg`) loses the
   alias chain (`Chain`, `ChainFile`, `LegacyPrefixes`, `Alias.Deprecated`,
   `ChainSpellings`) and every explicit `CHAT_`/`CUTLASS_` alias.
   `config.lookupFleet`, agentcore's `fleetEnv` and `tools.fleetEnv` read the
   canonical name only, and so does every reader that used to try a legacy
   spelling itself: the operator CLI's local health probe (`FLEET_TLS_MODE`,
   `FLEET_TLS_CERT_FILE`, `FLEET_TLS_DOMAIN`, `FLEET_SERVER_ADDR`),
   `fleet status`/`fleet diagnose`, `fleet cache-stats`, `fleet chat`,
   `internal/boxdoctor`, `internal/worktree`, the session log, the skills
   tree, `scripts/doctor.sh` and `scripts/lib/idle.sh`. The bare `DATA_DIR`
   that `fleet sched apikey` read goes too.
2. **The env-file loader does not admit a removed spelling** (the 22 it used
   to admit are gone from the allowlist).
3. **A removed spelling is unreadable everywhere.** Every lookup in
   `envreg` (`Get`, `Lookup`, `Unregistered`, `LookupUnregistered`) reads one
   as unset whatever the process environment holds (the shipped systemd unit
   exports every env-file line, so a stale value can sit in `os.Environ`).
   That covers fleet's own reads and every name the client bundle supplies.
   `envreg.RetiredAlias` recognises one: `CHAT_<X>` or `CUTLASS_<X>` when
   `FLEET_<X>` is a registered setting and the name is not a setting of its
   own, plus the bare `DATA_DIR`.
4. **In a client bundle, a removed spelling is an unset variable — and the
   upgrade refuses the bundle before installing.**
   - **Load** treats it exactly like any unset variable: `${CHAT_<X>:-d}`
     yields `d`; a bare `${CHAT_<X>}` where the load resolves the field
     (whole or embedded, e.g. `https://${CHAT_<X>}/api`) or
     `${CHAT_<X>:?…}` fails the load, and the error names the rename:
     `manifest references removed variable CHAT_<X>; rename it to FLEET_<X>`.
     A literal variable-name field (`api_key_env`, `enabled_env`,
     `enabled_groups`, `account_vars`, `identity_env`, the webhook
     `*_secret_env` fields) naming one reads as unset (no key, the gate stays
     closed). Every use, fatal or not, is logged at load with its
     replacement. There is no partial-load or entry-disabling machinery.
   - **One read-only scanner**, `clientconfig.RetiredManifestRefs`, finds
     every use: each `${…}` reference anywhere in the manifest (any section,
     whole or embedded) and each literal name in an env-name field. It
     loads nothing, applies no env file and writes nothing.
   - **`fleet update` refuses** a bundle with any use BEFORE installing: right
     after the new release verifies, it runs the NEW binary's
     `fleet validate-config --retired-only` (that scanner; exit 7 when it
     finds a use) against the box's bundle, and on a finding dies with each
     use, its field and its `FLEET_` replacement, "nothing was installed".
     `bootstrap.sh` runs the same check (`scripts/lib/retired-bundle.sh`).
     A release without `--retired-only` (a downgrade) still reads the old
     spellings, so the check only warns when it cannot run.
   - **`fleet doctor` and `fleet validate-config`** report a use as a
     blocking FAIL (the next upgrade refuses until it is renamed); doctor
     uses the scanner and never loads the bundle, so `--check` writes
     nothing.
   - A bundle deployed some other way (a manual install, Helm) fails to load
     with the precise rename instruction above.
   - The shell scripts that interpolate a manifest value (`bootstrap.sh`,
     `update.sh`, `doctor.sh`) share one resolver,
     `fleet_sandbox_resolve_scalar` in `scripts/lib/sandbox-bundle.sh`, which
     follows the loader's rules (a removed spelling is unset: `:-` gives the
     default, a required use fails with the rename) and never looks a
     removed spelling up; the list is the generated
     `scripts/lib/retired-env.sh`, and `TestShellResolverMatchesGo` keeps the
     two in step.
   - **Set in the environment** (process env, systemd unit, env file), a
     removed spelling is only reported: nothing reads it. `fleet serve` logs
     one warning per spelling (attributed to the env file when it is there),
     `fleet doctor` prints it, `fleet config explain FLEET_<X>` notes it,
     `fleet config explain CHAT_<X>` is an error naming `FLEET_<X>`, and
     `fleet config set-env CHAT_<X>` refuses.
   None of these prints a value.
5. **`fleet config migrate-env` is removed**, with its tests and docs. The
   boxes are migrated; an operator who finds a leftover line renames it by
   hand (`fleet env edit`, or `fleet config set-env` + `unset-env`).
6. **The MCP broker still keeps the `CHAT_`/`CUTLASS_` spellings of
   parent-owned names out of connector env** (`envreg.RetiredSpellings`). fleet
   no longer reads them, but a stale line on a box still holds the same value
   (a server token, a DSN), and a connector must not be able to claim it.

**Not changed.** The canonical `CHAT_`/`CUTLASS_` names ADR-0099 listed are
settings, not aliases, and stay: `CHAT_RATE_PER_MIN`, `CHAT_RATE_PER_DAY`, the
connector wire keys `CUTLASS_ALLOWED_DIRS`, `CUTLASS_IMAGE_OUTPUT`,
`CUTLASS_IMAGE_MODEL` (and the bundle-declared `CUTLASS_RUN_WORKDIR` and
friends), and the script inputs `CHAT_DB_*`, `CHAT_URL`, `CHAT_PORT`,
`CHAT_DSN`. The bare `PERSONA` / `PERSONA_DEFAULT` spellings stay.
`RetiredAlias` never reports a registered name.

## Enforcement

- `TestRetiredAliases` (envreg): the removed spellings are unregistered and
  map to their `FLEET_` key; the canonical `CHAT_`/`CUTLASS_` settings are not
  retired; no entry declares a retired spelling as an alias (the registry
  also panics at init on one).
- `TestNoUnregisteredLiteralReads` and the test-binary panic in `envreg.Get`
  fail on any code that reads a removed spelling.
- `TestExplainMatchesLoad` sets every removed spelling alongside the real
  ones and requires `Load` and `explain` to agree that none is read;
  `TestExplainNeverSelectsARemovedAlias`, `TestDoctorWarnsAboutRemovedAliases`
  and `TestConfigExplainNamesTheCanonicalKeyForARemovedAlias` pin the
  reporting.
- `registry_snapshot_test.go` records the deliberate allowlist and knob
  changes in its header.
- `TestEveryLookupReadsARetiredNameAsUnset` (envreg) covers every exported
  lookup; `TestRetiredRefIsAnUnsetVariable` and
  `TestRetiredManifestRefsFindsEveryUse` (clientconfig) pin the load
  behaviour and the scanner; `TestShellResolverMatchesGo` pins the shell
  resolver to the loader; `TestUpdateRefusesARetiredBundleBeforeInstalling`
  (scripts) builds the binary and pins the pre-install refusal;
  `TestDoctorBundleCheckWritesNothing` pins that doctor only scans.

## Consequences

- One spelling per setting: the loader, the readers and the docs agree by
  construction, which ends the drift ADR-0099 described.
- A box that still sets a removed spelling gets the setting's default, and a
  warning at boot and in `fleet doctor` that names the key to rename.
- `docs/CONFIGURATION.md` no longer lists aliases, and the Helm schema no
  longer has the `deprecated` alias properties. A `CHAT_<X>` key in Helm's
  `config.env` is still accepted by the schema (the map stays open for
  bundle variables), and the server warns about it at boot.

## Alternatives considered

- **Refuse to boot on a removed spelling SET in the environment.** Rejected:
  nothing reads it, so a warning that names the fix is enough; refusing would
  turn a harmless leftover line into an outage.
- **Never fail the load on a bundle's removed reference** (interpolate it as
  empty and disable whichever entry it breaks). Tried on #2081 and rejected:
  keeping that promise across every required field, embedded reference and
  section took ever more fragile machinery (matching validation errors to
  entries). Refusing the bundle at upgrade time, before install, keeps the
  real promise — an upgrade cannot take a box down — with one read-only
  scanner.
- **Keep `migrate-env` for one more release.** Rejected: it rewrites only
  aliases, and there are none left to rewrite.
