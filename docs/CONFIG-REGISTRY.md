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
| `Type` | `string`, `int`, `int64`, `float`, `bool`, `strict-bool`, `duration`, `url`, `csv`, `path`, `enum`, `base64` |
| `Default` | the documented default (prose in parentheses when it is derived) |
| `Description`, `Group` | the docs text and section |
| `Scope` | `server`, `cli`, `sandbox`, `broker`, `scripts`, `test` |
| `Secret` | redacted by `explain`, never printed by `migrate-env`, routed to `existingSecret` by the Helm docs |
| `ProcessOnly` | fleet's own env-file loader does not admit the name |
| `Chain`, `ChainFile` | the setting resolves `FLEET_<X>` → `CHAT_<X>` → `CUTLASS_<X>`; `ChainFile` names the legacy prefixes the env file admits. The chain aliases are derived, never spelled out |
| `Aliases` | other spellings (`Deprecated`; `EnvFile`, whether fleet's own loader admits it; `ReadBy`, the one reader of a reader-specific alias the server ignores) |
| `Enum` | accepted values of an `enum` |
| `Knob` | makes it a boot-validated typed knob: `Min`/`Max`, `ReadBy` (a consumer outside `config.Load`), `Lenient`/`Why` |
| `Helm` | the chart value that sets it (the explicit values↔settings mapping) |

The table holds 371 settings. 276 are admitted from the env file under their
canonical name (the old allowlist's 300 keys are those plus 22 legacy alias
spellings and the bare `PERSONA` / `PERSONA_DEFAULT`). The other 95 are read only
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

### `fleet config explain [KEY…] [--all] [--json] [--env-file F]`

Resolves each setting the way the server does — process env first, then the
env file (only for names its loader admits), across the canonical name and its
aliases in resolution order — and prints the effective value, its source
(`process env`, `env file`, or `default`), the spelling that supplied it, and
`DEPRECATED` when that spelling is a legacy alias. It also notes a value in the
env file the loader ignores, and a lower-precedence spelling that is also set.
Secrets print as `<redacted>`. No KEY lists every setting that is set; `--all`
lists every setting with its default. An unknown KEY is an error.

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
box's fleet unit (or a drop-in) loads the env file with `EnvironmentFile=`,
every line counts as the server's environment, not only the names fleet's own
loader admits; without such a unit an unadmitted line is reported as ignored.
A reader-specific alias is never reported as a setting's effective value,
only as a note.

### Redaction

`explain` prints nothing of a `Secret` entry (`<redacted>`), except a DSN
(`Type: dsn`), which keeps its host and database with the userinfo stripped.
Every other value that contains `://` has its userinfo and its credential-like
query parameters (token, key, secret, password, signature, auth, …) removed.
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
  the password of a key=value DSN.
- **Four more settings are secrets** (`OPENAI_CUSTOM_HEADERS`,
  `FLEET_WEBHOOK_URL`, `FLEET_WEBHOOK_BODY_TEMPLATE`, plus the internal
  `FLEET_DRAIN_OWNER_TOKEN`): the generated docs mark them, and the Helm
  schema points them at `config.existingSecret`. Nothing refuses them in
  `config.env`.

## Contradictions fixed on the way

- OPERATORS.md said the DB pool knobs default to "pgx defaults"; they default
  to 25 open / 5 idle / no idle reaping / 5 m lifetime. Its knob table now
  points at CONFIGURATION.md instead of keeping a second, partial copy.
- `Config.SandboxK8sNamespace`'s comment named `fleet-sandboxes` as the
  default; the backend takes the kubeconfig context's namespace, else the
  control plane's own, else `fleet-sandboxes` (as DEPLOYMENT-KUBERNETES.md
  already said).
