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
12 boxes `fleet config migrate-env` reports "nothing to rewrite", and `fleet
config explain` and `fleet doctor` are clean. So the removal needs no
compatibility window.

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
   That covers fleet's own reads and every name the client bundle supplies:
   `${…}` references and literal variable-name fields (`api_key_env`,
   `enabled_env`, `enabled_groups`, the webhook `*_secret_env` fields, …),
   which read as unset (no key, gate closed). The shell scripts that
   interpolate a manifest value (`bootstrap.sh`, `update.sh`, `doctor.sh`)
   share one resolver, `fleet_sandbox_resolve_scalar` in
   `scripts/lib/sandbox-bundle.sh`, which never looks a removed spelling up in
   the shell or the env file (the list is the generated
   `scripts/lib/retired-env.sh`).
4. **A removed spelling is reported, never silently ignored.**
   `envreg.RetiredAlias` recognises one: `CHAT_<X>` or `CUTLASS_<X>` when
   `FLEET_<X>` is a registered setting and the name is not a setting of its
   own, plus the bare `DATA_DIR`. Then:
   - `fleet serve` (any `config.Load`) logs one warning per removed spelling
     set in the process environment or the env file, naming the `FLEET_`
     key — once per spelling, attributed to the env file when it is there.
     It does not refuse to boot.
   - `fleet doctor` prints the same warning first, from the process
     environment, the systemd unit's environment and the env file.
   - `fleet config explain FLEET_<X>` adds a note when a removed spelling of
     it is set; `fleet config explain CHAT_<X>` is an error naming `FLEET_<X>`.
   - `fleet config set-env CHAT_<X>` refuses, naming `FLEET_<X>`.
   - A client bundle whose manifest references `${CHAT_<X>}` still loads:
     the reference is never read from the environment and interpolates as
     empty (or its `:-` default), even in the `${CHAT_<X>:?…}` form and in
     fields that otherwise require a value. The load logs a warning naming
     `${FLEET_<X>}`, and `fleet doctor` and `fleet validate-config` report
     it as a non-blocking advisory (as they do a removed spelling still set
     in the environment).
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

- **Refuse to boot on a removed spelling.** Rejected: every box is migrated,
  and a warning that names the fix is enough; refusing would turn a harmless
  leftover line into an outage.
- **Keep `migrate-env` for one more release.** Rejected: it rewrites only
  aliases, and there are none left to rewrite.
