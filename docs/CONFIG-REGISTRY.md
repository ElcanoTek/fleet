# Configuration registry (#2053)

Every environment variable fleet reads is declared **once**, in
[`internal/config/envreg/table.go`](../internal/config/envreg/table.go). The
env-file allowlist, the typed-knob validation, the generated reference
([CONFIGURATION.md](CONFIGURATION.md)), the Helm schema's `config.env`
properties, `fleet config explain`, `fleet config migrate-env` and the alias
warnings in `fleet doctor` are all derived from that one table, and a lint rule
plus two tests keep anything from reading the environment around it.

This page records what shipped, how to use it, and where it deviates from the
issue.

## What shipped

### The registry (`internal/config/envreg`)

A leaf package (standard library only) so every package can import it —
including `internal/agentcore`, `internal/creds` and `internal/mcp`, which
`internal/config` itself imports, so the table cannot live in `config`
proper. Each `Entry` carries:

| Field | Meaning |
| --- | --- |
| `Name` | canonical spelling |
| `Type` | `string`, `int`, `int64`, `float`, `bool`, `strict-bool`, `duration`, `url`, `dsn`, `csv`, `path`, `enum`, `base64` |
| `Default` | the documented default; prose in parentheses only for a run-time decision, and such prose never names another setting (`TestDerivedDefaultsAreStructured`) |
| `DefaultFrom`, `DefaultDerive`, `DefaultFromCanonical` | an **inherited** default: when unset, the setting takes `DefaultFrom`'s resolved value (optionally transformed by `DefaultDerive`, which mirrors the reader; `DefaultFromCanonical` when the reader reads only the parent's canonical spelling) |
| `Description`, `Group` | the docs text and section |
| `Scope` | `server`, `cli`, `sandbox`, `broker`, `scripts`, `test` |
| `Secret` | redacted by `explain`, never printed by `migrate-env`, routed to `existingSecret` by the Helm docs |
| `ProcessOnly` | fleet's own env-file loader does not admit the name |
| `Chain`, `ChainFile` | the setting resolves `FLEET_<X>` → `CHAT_<X>` → `CUTLASS_<X>`; `ChainFile` names the legacy prefixes the env file admits. The chain aliases are derived, never spelled out |
| `Aliases` | other spellings (`Deprecated`; `EnvFile`, whether fleet's own loader admits it; `ReadBy`, the one reader of a reader-specific alias the server ignores) |
| `Enum` | accepted values of an `enum` |
| `Knob` | makes it a boot-validated typed knob: `Min`/`Max`, `ReadBy` (a consumer outside `config.Load`), `Lenient`/`Why` |
| `Helm` | the chart value that sets it (the explicit values↔settings mapping) |

The table holds 401 settings. 276 are admitted from the env file under their
canonical name (the old allowlist's 300 keys are those plus 22 legacy alias
spellings and the bare `PERSONA` / `PERSONA_DEFAULT`). The other 125 are read only
from the process environment and were in no table before: CLI, Kubernetes,
platform and harness variables, and the operator-facing inputs of the shell
scripts. 149 settings resolve through the legacy-prefix chain, which with 9
explicit spellings makes 307 deprecated aliases.

### Derived, not maintained

- **`allowedEnvVars`** (the env-file allowlist) is every entry that is not
  `ProcessOnly` plus every alias marked `EnvFile`. The hand-kept 450-line map
  is gone.
- **`envKnobs`** (the typed-knob table behind boot validation, hot-reload and
  `fleet validate-config`) is every entry with a `Knob`.
- **`lookupFleet`** (config) and **`fleetEnv.lookup`** (agentcore) take their
  spellings from `envreg.ChainSpellings`, so a chain read of an undeclared
  setting fails the test suite.

Behaviour is pinned byte for byte by
[`registry_snapshot_test.go`](../internal/config/registry_snapshot_test.go),
a verbatim copy of the two tables the registry replaced:
`TestAllowlistMatchesPreRegistrySnapshot` (the derived allowlist equals the
old map exactly), `TestAllowlistAdmitsEveryOldKeyFromAFile` (every old key,
set only in an env file, reaches the process through the real `applyEnvFile`
path, and a process-env value still wins), and
`TestKnobsMatchPreRegistrySnapshot` (kind, chain, bounds, scope, consumer and
leniency of every knob row). The existing loader, reload, knob-sweep and
drift suites pass unchanged.

### One way to read the environment

| Accessor | Use |
| --- | --- |
| `envreg.Get(name)` / `envreg.Lookup(name)` | a registered name (canonical or alias) |
| `envreg.Unregistered(name)` / `envreg.LookupUnregistered(name)` | a name that comes from **data** — the client bundle's manifest (connector credentials, webhook signing secrets, provider API keys, `${VAR}` interpolation), a per-account suffix variant, a name passed on a flag |
| `envreg.ChainSpellings(canonical)` | the legacy-prefix family of a `Chain` setting |

`Get` of an unregistered name **panics in a test binary** (`testing.Testing()`)
and, in production, **logs a `BUG:` line once per name** and returns the value
anyway — a missed registration can never take a deployment down, and it is
never silent. `TestNoUnregisteredLiteralReads` walks every non-test Go file and
fails on a literal name passed to `Get`/`Lookup` or to one of the repo's
env-reading helpers (`envOrFile`, `envInt`, `fleetEnv`, `getenvFleet*`, …)
that is not registered, and on **any** literal passed to `Unregistered`, so the
common mistakes are caught statically without executing the read.

All ~140 direct reads outside `internal/config` moved, each keeping its key,
default and parsing.

**The lint rule.** `.golangci.yml` enables `forbidigo` with one pattern,
`^os\.(Getenv|LookupEnv|Environ)$`, and these exceptions:

| Exception | Why |
| --- | --- |
| `internal/config/` | owns the environment: the accessor itself, the env-file loader (snapshot/restore), the DSN resolver |
| `_test.go` | tests stage scenarios with the environment directly |
| `os.Environ` in `internal/admincli/{backup,update}.go` | subprocess env plumbing: the whole environment handed to `pg_dump` / `update.sh` |
| `os.Environ` in `internal/agentcore/redact.go`, `internal/mcpbroker/redact.go`, `internal/tools/browserbase_live_view.go`, `internal/admincli/diagnose.go` | secret redactors snapshot every **value** as a literal to scrub; diagnose also lists set **names** |
| `os.Environ` in `cmd/fleet/mcp_broker_runtime.go`, `internal/creds/creds.go` | the broker's divestment scan of connector variables, and per-account suffix discovery |

The sandbox env builder needed no exception: it never reads the host
environment.

### Generated docs and Helm schema

`make config-docs` runs the generator
(`go test ./internal/config/envreg -run TestGeneratedFilesAreUpToDate -update`,
the repo's `-update` golden-file convention) and rewrites:

- **[CONFIGURATION.md](CONFIGURATION.md)** — every setting by section, with
  type, default, scope, secret, env-file admission, boot validation and bounds,
  aliases (deprecated ones marked, with whether the env file admits each), and
  the Helm value that sets it; plus a table of every deprecated alias.
- **the `config.env` subtree of
  [`deploy/helm/fleet/values.schema.json`](../deploy/helm/fleet/values.schema.json)** —
  one property per control-plane setting (server, sandbox and broker scopes),
  with its description, the chart value that sets it, `deprecated: true` on
  the legacy spellings, and a pointer to `existingSecret` for secrets. Every
  other byte of the hand-written schema is left as it is
  (`RewriteHelmSchema` splices only that member).

`TestGeneratedFilesAreUpToDate` fails when either file differs from what the
table generates. `TestHelmTemplateEnvMatchesRegistryMapping` checks the
mapping both ways: every `- name:` the chart's Deployment sets is a registered
canonical name with a `Helm` mapping, and every entry with a mapping is set by
the template.

### `fleet config explain [KEY…] [--all] [--json] [--no-unit] [--env-file F]`

**The contract.** `explain` reports each setting's **configured value** and
its **source**. It draws only from the sources it can see, highest first:

1. **The systemd unit's own environment**, on a systemd box: `Environment=`,
   then each `EnvironmentFile=` (a later file wins), minus
   `UnsetEnvironment=`. This is what the service process starts with, so it
   outranks the shell running `explain`. `--no-unit` leaves it out. The CLI
   reads it with `systemctl show -p LoadState -p Environment -p
   EnvironmentFiles -p UnsetEnvironment <unit>`. Without systemctl it parses
   the unit files (see below). An `EnvironmentFile=` the CLI cannot read is
   listed so the operator can re-run as root.
2. **The shell's process environment.**
3. **The env file**, as fleet's own loader reads it. With a unit, this
   applies only when the unit's `FLEET_ENV_FILE` names that file. The
   standard unit unsets `FLEET_ENV_FILE` and exports the file through
   `EnvironmentFile=`, so its lines arrive as source 1.
4. **The setting's alias spellings.**
5. **Its default**, including one **inherited** from another setting
   (`DefaultFrom`).

It is **not** the running server's in-memory configuration. Admin overrides
made in the app, values a running process has not reloaded, and run-time
decisions are outside it. The usage text, this page, CONFIGURATION.md and the
JSON field names say exactly that. The JSON fields are:
- `sources` (`process_env`, `env_file`, `env_file_read_by_fleet_loader`,
  `systemd_unit`);
- per setting: `configured_value`, `source`, `source_detail`, `spelling`,
  `inherited_from`, `inherited_source`, `deprecated`, `secret`, `notes`.

**One resolution.** `explain` resolves through the **same code** as
`config.Load`, in [`internal/config/resolve.go`](../internal/config/resolve.go):

1. **Merge, per variable name** (`envLayers.get`):
   - the unit's value, if it sets the name (an `UnsetEnvironment=` name is
     absent);
   - else a name present in the process environment, even when its value is
     empty;
   - else the env file's value, if the name is admitted.

   `applyEnvFile` writes the last two steps into the process environment at
   boot. Inside the service, the unit's environment *is* the process
   environment.
2. **Pick, across a setting's spellings** (`resolveSpellings`). The first
   spelling with a non-empty value wins. A reader-specific alias is not among
   the spellings.
3. **Default** (`resolveSetting`). An unset setting with `DefaultFrom`
   resolves its parent through this same function, recursively, then applies
   `DefaultDerive`. Otherwise it takes its `Default`.

`explain` prints the configured value; its source, with the unit's
`Environment=` or `EnvironmentFile=<path>` as detail; the spelling; and
`DEPRECATED` for a legacy alias. An inherited default prints as, for example,
`vendor/my-model` with source `default (inherited from FLEET_MEMORY_MODEL ←
FLEET_METADATA_MODEL ← FLEET_TITLE_MODEL, set in env file as
FLEET_TITLE_MODEL)`. Notes cover:
- a shell value the unit overrides ("your shell sets X to a different value,
  but the service uses the systemd unit's");
- a name the unit removes;
- a file value masked by a higher source (even an empty one);
- a file value the loader does not admit;
- a lower-precedence spelling that is also set;
- a reader-specific alias.

Secrets print as `<redacted>`. With no KEY, `explain` lists every setting
that is set **or carries a note**, so an entry whose only spelling set is
`CHAT_TLS_MODE` still appears. `--all` lists every setting with its default.
An unknown KEY is an error.

**Inherited defaults.** These entries use `DefaultFrom`, found by grepping
`config.go` and the readers for derived defaults:
- the model chain: `FLEET_METADATA_MODEL` ← `FLEET_TITLE_MODEL`, then the
  error-analysis, memory, recurring-task and library-prompt models ←
  metadata, and the memory graph ← memory;
- `ORCHESTRATOR_SERVER_TOKEN` ← `FLEET_SERVER_TOKEN`, canonical spelling only,
  as the gateway reads it;
- the drain sentinel: `FLEET_UPDATE_DRAIN_FILE` ← `FLEET_DRAIN_FILE` ←
  `<FLEET_ENV_FILE>.update.drain`;
- `FLEET_AUDIT_DIR` ← `<FLEET_DATA_DIR>/audit`;
- `FLEET_OIDC_REDIRECT_URI` ← the public origin's `/api/auth/oidc/callback`;
- `FLEET_SANDBOX_WARM_SIZE` ← `FLEET_MAX_CONCURRENT_AGENTS` clamped to 2..8.

`FLEET_TITLE_MODEL`'s default is now the literal everyday-tier slug, pinned to
`agentcore.DefaultCoreModel` by `TestTitleModelDefaultIsTheCoreModel`.

`TestInheritedDefaultsMatchTheReaders` covers every entry with `DefaultFrom`;
a new one fails until it is added. For each, it compares `explain` against
the code that actually uses the value, with nothing set, with each ancestor
set, and with the setting itself set:
- for Config fields: `config.Load`;
- for the rest: `webui.ConfigFromEnv`, `httpapi.UpdateDrainFile`,
  `tools.AuditDir`, `agent.WarmPoolSize`.

`TestDerivedDefaultsAreStructured` fails when a `Default` text names a
setting. It also fails when a description says "defaults to / falls back to /
inherits / derived from `<SETTING>`" without the matching `DefaultFrom`.

`TestExplainMatchesLoad` pins the two against each other. For six settings,
it applies every combination of {absent, process env, process env
present-but-empty, env file, empty process + env file} across all their
spellings: canonical, CHAT_/CUTLASS_, the bare `PERSONA_DEFAULT`, and the
reader-specific `CHAT_TLS_MODE` and `DATA_DIR`. That is over 1,000 cases.
Each case asserts that `explain`'s value and source are what `Load`
actually produced.

### Deprecating the aliases ([ADR-0099](adr/0099-deprecate-the-chat-cutlass-env-aliases.md))

- **`fleet doctor`** prints one warning per deprecated spelling set in the
  process environment or the server env file, naming the canonical key, before
  it runs the box checks (and on the status fallback). It never prints a value.
- **`fleet config migrate-env [--write]`** rewrites the env file to canonical
  keys. Dry run by default (line numbers and key names, `KEY=…`, never values).
  `--write` copies the file to `<file>.bak-<UTC timestamp>` (0600, owner kept)
  and then writes atomically through the creds writer (temp + fsync + rename,
  0600, owner kept). Only the key token of each line changes; comments, blank
  lines, order, `export ` prefixes and value text are untouched. An alias line
  whose canonical key already holds the same value is dropped. It refuses —
  writing nothing — when two spellings of one setting have different values,
  or one alias is assigned twice with different values. An alias fleet's own
  env-file loader does not admit is rewritten too: on the standard install the
  systemd unit's `EnvironmentFile=` exports every line, so the line has been
  effective there, and the plan says so. The one exception is a
  *reader-specific* alias (`Alias.ReadBy`: `CHAT_`/`CUTLASS_TLS_*`, read only by
  the CLI's local health probe), which the server ignores; it is left in place
  and reported, because renaming it would start applying it to the server.

`fleet config explain` and `fleet doctor` follow the same rules. When the
fleet unit loads the env file with `EnvironmentFile=`, every line counts as
the server's environment, not only the names fleet's own loader admits;
without such a unit an unadmitted line is reported as ignored. A
reader-specific alias is never reported as a setting's configured value, only
as a note.

The unit's environment comes from systemd itself (`systemctl show -p
LoadState -p Environment -p EnvironmentFiles -p UnsetEnvironment
fleet.service`, which already merges drop-ins; `LoadState=not-found` means
no unit). Only when `systemctl` is missing or fails (a container, no systemd)
does the CLI parse the unit files, the way systemd resolves them
(`config.UnitEnvFromFiles`):
- the base unit is the first one found in the search order `/etc/systemd/system`,
  `/run/systemd/system`, `/usr/local/lib/systemd/system`,
  `/usr/lib/systemd/system`, `/lib/systemd/system`;
- drop-ins come from every directory, a higher-priority directory masking a
  drop-in of the same name, and apply in file-name order;
- an empty `Environment=`, `EnvironmentFile=` or `UnsetEnvironment=` resets
  that list;
- only `[Service]` counts.

`Environment=` values are split the way systemd splits them (quotes and
backslash escapes). Tests drive all of this with fake `systemctl` output.

The unit name is `$FLEET_SERVICE_NAME` or `fleet`.

### Redaction

`explain` prints nothing of a `Secret` entry (`<redacted>`), except a DSN
(`Type: dsn`), which keeps its host and database with the userinfo stripped.
Every other value that contains `://` has its userinfo and its credential-like
query parameters (token, key, secret, password, signature, auth, …) removed.
A parameter name is matched **decoded**, so `%74oken` is `token`, but it is
printed as written.
`config.RedactDSN` does this, and `fleet status`, `fleet diagnose` and `fleet
backup` share it. `TestCredentialValuesAreSecret` requires every entry whose
name says it is a credential, and every DSN, to be `Secret`, except a short
list with written reasons (a public key, a key-file path, a Secret object's
name). `OPENAI_CUSTOM_HEADERS`, `FLEET_WEBHOOK_URL` and
`FLEET_WEBHOOK_BODY_TEMPLATE` are pinned as secrets, because they can carry a
credential without saying so in their name. Tests set every registered
setting to a credential-shaped URL and assert that neither the table nor the
JSON output prints any of it.

## Removing the aliases

When every box has run `fleet config migrate-env --write` (and `fleet doctor`
is quiet on each), the follow-up PR removes alias support. Every touch point
carries `LEGACY-ENV-ALIAS: remove after migrate-env`; `git grep -n
LEGACY-ENV-ALIAS` lists them all. The checklist:

1. **Delete** `internal/config/legacy_aliases.go` and
   `internal/admincli/legacy_env.go`, and their tests in
   `internal/config/explain_test.go` / `internal/admincli/config_explain_test.go`
   (the parts marked).
2. **Unhook** the `migrate-env` case in `cmdConfig` and the
   `warnDeprecatedEnvAliases` call in `cmdDoctor`; drop both from the usage
   text.
3. **Registry:** delete `LegacyPrefixes`, `Chain`, `ChainFile`,
   `Alias.Deprecated`, `expandChain`, `isLegacyPrefix`, `DeprecatedAliases`,
   `ChainSpellings` and the `fileCHAT`/`fileCUTLASS`/`fileBoth` shorthands,
   and every explicit `CHAT_*`/`CUTLASS_*` alias in the table (each is marked).
   Keep the canonical `CHAT_`/`CUTLASS_` settings (`CHAT_RATE_PER_*`, the
   `CUTLASS_*` connector wire keys, the `CHAT_DB_*` script inputs).
4. **Readers:** `lookupFleet` and `fleetEnv.lookup` read the canonical name
   only; `EnvAliases` returns its argument alone; the explicit alias reads in
   `internal/tools/env.go`, `internal/admincli/idle.go` and `status.go`,
   `internal/boxdoctor`, `internal/worktree`, `internal/agent`
   (`CUTLASS_LOG_FILE`), `internal/clientconfig/builtin_skills.go` and
   `internal/chattui` go, as do the marked `CHAT_`/`CUTLASS_` reads in
   `scripts/doctor.sh` and `scripts/lib/idle.sh`.
5. **Snapshot tests:** the allowlist loses the 22 file-admitted alias
   spellings and the knob rows lose their chain flag. Update
   `registry_snapshot_test.go` in the same PR with a line in its header
   saying so — that diff is the record of the deliberate change.
6. **Tests that set legacy spellings** (the "lifted parity" tests and the
   alias tests in `internal/config`, `internal/agentcore`) are deleted or
   switched to `FLEET_` names.
7. `make config-docs`; supersede ADR-0099 with the removal ADR; drop the
   "Deprecated aliases" notes in OPERATORS.md.

## Deviations from the issue

- **The registry is a Go table, not YAML compiled by `go generate`.** The
  issue left this open. A typed Go table gets compile-time checking of every
  field, needs no generator to read it, and the two outputs that do need
  generating (the doc, the schema) are produced from it by a test-driven
  generator, matching the repo's `-update` convention.
- **`Scope` adds `cli`** to the issue's server/sandbox/broker/scripts/test,
  because about twenty settings are read only by operator CLI verbs; and
  `Helm` is a separate field rather than a scope, since a setting can be both
  server-scoped and set by the chart.
- **Only the `config.env` part of the Helm schema is generated.** The rest of
  `values.schema.json` describes chart values (`sandbox.image`, `workspace.*`,
  `networkPolicies.*`), not environment variables; the mapping between the two
  is the registry's `Helm` field, checked against the Deployment template by
  `TestHelmTemplateEnvMatchesRegistryMapping`.
- **Removal is a follow-up PR, not a dated window** (owner direction): every
  box is owner-controlled and will be migrated first. See ADR-0099.
- **`internal/config/dsn.go` is unchanged.** The DSN keys are registered
  exactly as they are read today; `ProcessEnv` keeps reading `os.Getenv` inside
  `internal/config`, which the lint rule allows.

## Behaviour changes (deliberate, small)

- **Order of multi-problem knob reports.** The derived knob table is in
  registry order, so when several out-of-loader knobs are malformed at once,
  `fleet validate-config` and the boot error list them in that order instead
  of the old table's. Which knobs are reported, and their messages, are
  unchanged.
- **`fleet doctor` prints the alias warnings** before its checks.
- **Helm: `config.env` values must be scalars.** A map or list value used to
  pass the schema and render as Go map syntax; it is now refused by
  `helm lint`/`install`. String, number and boolean values, for known and
  unknown keys alike, are accepted as before.
- **gosec taint suppressions.** gosec's taint analysis treated `os.Getenv` as
  an untrusted source; reads now go through `envreg`, so 35
  `//nolint:gosec` directives that justified operator-set config flowing into
  a path or log line became unused and were removed (nolintlint requires it).
  The values are the same operator config they always were.
- **A read of an undeclared name** logs `BUG: envreg: …` once in production;
  it used to be silent.
- **DSN redaction is stricter.** `fleet status`, `fleet diagnose` and `fleet
  backup` errors used to strip only a URL's userinfo; they now share
  `config.RedactDSN`, which also removes credential-like query parameters and
  the password of a key=value DSN. It is one function: ADR-0098's
  (`internal/config/dsn.go`: pgx's `password`/`sslpassword`/`passfile` and
  key=value DSNs) widened to any `scheme://` URL and to credential-shaped
  query keys matched decoded, each masked as `***`.
- **The keys ADR-0098 added are registered**: the libpq/pgx `PG*` target
  variables `fleet db consolidate` refuses (`PGHOST`, `PGPORT`,
  `PGDATABASE`, `PGUSER`, `PGSERVICE`, `PGSERVICEFILE`), and the script
  inputs `FLEET_IDENTITY_BIN`, `FLEET_REDACT_CHAT_PH` and
  `FLEET_REDACT_SCHED_PH`; `fleet db consolidate` reads them through `envreg`.
- **Four more settings are secrets** (`OPENAI_CUSTOM_HEADERS`,
  `FLEET_WEBHOOK_URL`, `FLEET_WEBHOOK_BODY_TEMPLATE`, plus the internal
  `FLEET_DRAIN_OWNER_TOKEN`): the generated docs mark them, and the Helm
  schema points them at `config.existingSecret`. Nothing refuses them in
  `config.env`.
- **`explain`'s JSON changed shape** (the command is new in this PR, so
  nothing consumes it yet). The top level is `sources` + `settings`, and each
  setting's value is `configured_value`, not `value`. The fields name what
  `explain` actually reports.
- **Two readers gained an exported accessor** so the inheritance cross-check
  can call the real code. `tools.AuditDir` is the bash audit-log directory
  logic, extracted unchanged from `auditBashInvocation`. `agent.WarmPoolSize`
  wraps `resolveWarmSize`.
- **Boot applies the env file through the shared merge.** `applyEnvFile` now
  reads the whole file, then applies each admitted name that the process env
  does not already hold, instead of applying every line and then restoring
  the process winners. The resulting environment is the same. The one
  difference is the error path: if the file fails to parse partway through
  (for example, a line over the scanner limit), nothing from it is applied,
  and Load fails either way.

### Shell scripts are scanned too

`TestShellScriptReadsAreRegistered` extends the "no unregistered reads" rule
to every shell script under `scripts/` and `deploy/`: `*.sh` files and files
with a sh or bash shebang. A name counts as read from the environment in
either of these cases:
- **Nothing assigns it**: not the script, and not a sourced `scripts/lib`
  file. For a lib file itself, no script that sources it assigns it either.
- **It is read with a default expansion before the script assigns it**:
  `${X:-d}`, `${X:=d}` or `${X:?}` on or before the script's first
  assignment of `X`. This is the input idiom `X="${X:-d}"`, including
  `X="${X_ARG:-${X:-d}}"`.

Perl `$ENV{X}` in embedded programs counts unless the script passes `X`
itself. Every such name must be registered or appear in one of two commented
lists:
- `shellInternal`: names set by bash, the OS, sudo, CI or the tools fleet
  drives;
- `scriptLocals`: one entry, `update.sh`'s `env_get` default.

Awk and Perl tokens such as `$NF` and `$_` are excluded explicitly.

The scan found 21 unregistered inputs, all now registered:
- the e2e harness's `ORCH_PORT`, `WEB_PORT` and `E2E_*`, and `CANARY_MODEL`;
- `HTTPAPI_TEST_DATABASE_URL`;
- bootstrap's `SCHED_DB_NAME`, `SCHED_DB_USER` and `SCHED_DB_PASSWORD`;
- doctor's `SRC_DIR`;
- `IMAGE_NAME`, `FLAG_DIR`, `MIGRATION_LINT_BASE` and `FREEZE_VERSION`.

### Registry defaults are checked against the code

`TestRegistryDefaultsMatchCodeFallbacks` walks every non-test Go file. For
each call to an env parse helper with a hard-coded fallback, it evaluates the
fallback and requires the registry's `Default` to say the same. It currently
checks 116 call sites. The helpers are Load's typed readers and string helpers,
`EnvKnobInt`/`EnvKnobBool`, httpapi's `envDuration`/`envInt`, agentcore's
`lookupFloatDefault`/`lookupBoolDefault`, the harnesses' `envOr`, and
webpush's `envBoolDefaultTrue`. A fallback is evaluated when it is made of
literals, same-package constants, time units, arithmetic or conversions.
Prose defaults (`(derived from …)`) and non-constant fallbacks are skipped.

`TestRegistryDefaultsOfHandParsedSettings` pins the settings parsed by hand
without such a helper, each with its read site:
- `FLEET_UPDATE_WAIT_SECONDS` (600, which had no Default until Codex round 2);
- the runner's wall clock and concurrency;
- notify's timeout, retries and SMTP port;
- the sandbox stats interval and Kata overhead;
- agentcore's tool-output cap, model-cache TTL, retry budget and first-chunk
  deadlines;
- the workspace download cap;
- the scheduler rate limits;
- the listener addresses and timezones;
- the OIDC scopes;
- the service name.

## Contradictions fixed on the way

- OPERATORS.md said the DB pool knobs default to "pgx defaults"; they default
  to 25 open / 5 idle / no idle reaping / 5 m lifetime. Its knob table now
  points at CONFIGURATION.md instead of keeping a second, partial copy.
- `Config.SandboxK8sNamespace`'s comment named `fleet-sandboxes` as the
  default; the backend takes the kubeconfig context's namespace, else the
  control plane's own, else `fleet-sandboxes` (as DEPLOYMENT-KUBERNETES.md
  already said).
