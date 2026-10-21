# MCP bundle env contract: `${FLEET_WORKSPACE}`, `${FLEET_USER_EMAIL}`, `MCP_VARIANT_CLIENT`, `identity_env`, interactive critical-tool staging

Design note for the fleet-side support that bundle-provided Python MCP servers
(carried by out-of-repo client bundles) need from the platform. Canonical
server code lives in each bundle's upstream source repo; bundles mirror it and
sync from there rather than patching locally. Everything here is
client-agnostic mechanism — the client-specific names/values stay in the
bundle manifest.

## What shipped

### Scheduled task inputs and `${FLEET_TASK_ID}`

`POST /v1/tasks` accepts optional `file_names` paired 1:1 with `files`.
Fleet keeps the uploaded storage names collision-safe, then copies each input
into the dedicated run's `${FLEET_WORKSPACE}/inputs` directory under its
logical name before MCP subprocesses start. Bundle env maps such as
`CUTLASS_INPUT_DIR: "${FLEET_WORKSPACE}/inputs"` therefore agree with filenames
referenced in an intake-generated prompt.

Bundles may also reference the reserved `${FLEET_TASK_ID}` token in an MCP env
value. It is preserved at bundle load and replaced with the scheduled task UUID
only for that dedicated run; shared/interactive spawns drop token-bearing keys.

### 1. Reserved `${FLEET_WORKSPACE}` manifest-env token

The servers key several behaviors on writable directories passed via env:
`CUTLASS_RUN_WORKDIR` (cross-restart run ledger + "managed run" detection,
which arms e.g. the SendGrid fail-closed recipient allowlist),
`CUTLASS_REPORT_DIR`, `CUTLASS_INPUT_DIR`, `DEAL_SHEET_OUTPUT_DIR`. A bundle
cannot hardcode those paths, and plain `${VAR}` interpolation can only see the
operator's static deployment environment (the process env plus the
`FLEET_ENV_FILE` env file — never a per-run directory).

A bundle may now write, on a **stdio** server entry:

```yaml
env:
  CUTLASS_RUN_WORKDIR: "${FLEET_WORKSPACE}"
  CUTLASS_REPORT_DIR: "${FLEET_WORKSPACE}/reports"
```

The bare token is RESERVED: both interpolation passes leave it intact (it is
never resolved from the process env — an exported `FLEET_WORKSPACE` var cannot
hijack it — and never blanked). Only the bare spelling exists: any
colon-suffixed form (`${FLEET_WORKSPACE:-fallback}`, `${FLEET_WORKSPACE:?msg}`)
fails the bundle load — the token takes no default, and resolving it like an
ordinary env var would silently defeat the substitution below. The same rule
covers `${FLEET_TASK_ID}`. The spawn paths substitute it at
subprocess-launch time:

| Spawn path | Substituted value |
|---|---|
| Boot-time catalog (`agent.BuildMCPClient`), the `fleet mcp-broker` process, hot reload, load-on-demand onto a shared client | `SharedMCPWorkspaceDir()` — one stable `<workspace-root>/mcp-shared` dir per deployment |
| A scheduled task with an explicit `mcp_selection` (dedicated per-run client) | `<workspace-root>/mcp-runs/task-<id>` — one stable dir per task occurrence (row id): every retry of that occurrence re-mounts it and its ledger; a recurring successor, re-run or clone gets its own. Not pruned; creation failure fails the run's MCP setup (no shared-dir fallback). Every sandbox (podman and kubernetes) mounts `mcp-runs/` **read-only** over the read-write workspace mount, so a sandbox can read a run dir but never write, rename or plant anything in one, even while a connector is writing to it. As defense in depth, Fleet also resolves the run dir through an `os.Root`, refuses a symlinked `mcp-runs/` or run dir, unlinks any symlink, FIFO, socket or device planted at its top level (the start-of-run ledger read is also `O_NOFOLLOW`/`O_NONBLOCK`, regular-file-only and size-bounded), and rebuilds `inputs/` from the task's current attachments every attempt (the ledger beside it is kept) |

`<workspace-root>` is `FLEET_WORKSPACE_ROOT` (legacy `CHAT_`/`CUTLASS_`
aliases honored), else `./workspace` under the process cwd
(`/var/lib/fleet/workspace` under `deploy/fleet.service`). `fleet serve`
resolves it to an absolute path once at boot and exports it (a config reload
keeps that absolute value; changing the root takes a restart), so the default
needs no configuration and setting the variable is purely an override. A spawn path with no directory to offer
drops the token-bearing keys so the server sees the var as **unset** (its
documented inert posture) — never a literal token, never an empty string.

### 1a. Reserved `${FLEET_WORKSPACE_ROOT}` — the root itself, on every path

`${FLEET_WORKSPACE}` is the connector's *ledger* directory. It is not the
directory the run's files live in: a scheduled run's sandbox works at the
workspace root (`task.workspace_path`, the same `<workspace-root>` above), and
the per-run `mcp-runs/task-<id>/` dir is named by a task UUID the
model cannot know from inside the sandbox. A connector that allowlists the
files it may read — the SES sender's `content_file` / `attachments`
(`CUTLASS_ALLOWED_DIRS`) — therefore rejected every path the model could name:
the workspace root ("outside allowed directories"), the sandbox's own `/tmp`
(a different filesystem from the server's), and the per-run dir of an earlier
attempt. The Reklaim health scan spent 20 turns per run on this before sending
a hand-typed stub instead of its 350 KB report.

A bundle may therefore also write, on a **stdio** server entry:

```yaml
env:
  CUTLASS_RUN_WORKDIR: "${FLEET_WORKSPACE}"
  CUTLASS_ALLOWED_DIRS: "${CUTLASS_ALLOWED_DIRS}:${FLEET_WORKSPACE_ROOT}"
```

| Spawn path | Substituted value |
|---|---|
| **Every** path — boot catalog, hot reload, load-on-demand, `fleet mcp test`, broker scopes, the dedicated per-run client | `agentcore.WorkspaceRootDir()` — the absolute `<workspace-root>` |

There is always exactly one root, so the token is never dropped and never
blanked, and substituting it creates nothing on disk (unlike the other two
workspace dirs it does not mint anything, so a root-only reference does not
trigger a per-run dir). The bare spelling is reserved like the others: it is
never resolved from the process env at bundle load — the token shares its name
with the operator knob on purpose, and the spawn paths resolve it through the
same lookup that knob feeds, so the two can never disagree — and any
colon-suffixed form fails the load. Admitting the root means the connector can
read anything under the shared scheduled workspace; that is the same view the
sandbox already has (fleet#1543 tracks per-task isolation of that root).

**Honest scope.** On the *shared* spawn paths (the boot roster the broker child
keeps for tool discovery, hot reload, `fleet mcp test`, and load-on-demand onto
a shared client) MCP subprocesses are host-side and process-lifetime, so the
shared substitution is a *per-deployment* directory:
managed-run detection is on for everything the server process spawns, and
run-ledger entries persist across runs (a dedupe window, not a per-run
ledger). True per-run ledger semantics exist only on the dedicated-client path
(scheduled `mcp_selection` runs). Per-run dirs are deliberately not deleted at
run end — the ledger is post-run evidence. Bundles choose per server whether
to opt in at all: a server whose entry never references the token is
completely unaffected.

Production `fleet serve` runs broker mode, where an interactive turn opens its
own MCP scope and spawns fresh subprocesses for that turn — so there the
substituted directory is the **per-conversation workspace**, the same one
`bash`/`run_python` work in, not the per-deployment dir. The per-deployment
directory is what the shared paths above get.

### 1b. Reserved `${FLEET_USER_EMAIL}` / `${FLEET_USER_NAME}` — the signed-in user of a chat turn

A connector that writes something addressed to a person — the deal-sheet email
a bundle's `finish_plan_booking` sends, which should greet the trader by first
name and default its recipient to the trader — needs to know who is at the
keyboard. The model must not be the source (an argument is model-authored, so
an injected document could redirect the email), and the operator's env is one
value for every user. So a bundle may write, on a **stdio** server entry:

```yaml
env:
  DEAL_SHEET_DEFAULT_TO: "${FLEET_USER_EMAIL}"
  DEAL_SHEET_GREETING_NAME: "${FLEET_USER_NAME}"
```

| Token | Value |
|---|---|
| `${FLEET_USER_EMAIL}` | The turn sender's normalized (lowercased) email, exactly as fleet authenticated it. |
| `${FLEET_USER_NAME}` | The first name that user **saved for themselves** in Settings → General → "Your first name" (`PUT /me/name`, stored in `users.given_name`). Unset when they have not saved one. It is **never derived from the email**: fleet's sign-in (password, OIDC from the Elcano Auth service, the `elcano_auth` cookie) carries an email and no name claim, and `jsmith@…` is not "Jsmith". Read when the turn starts, so a newly saved name applies from the next turn. |

| Spawn path | What the server sees |
|---|---|
| An **interactive chat turn's** scope (production broker mode), when a signed-in person sent the turn — typed it or had it drained from their input queue — and the turn fleet resumes in their conversation after one of their approval cards settles (ADR-0083; it continues their own turn, even when the card settled by timing out) | The sender's (conversation owner's) email and saved first name |
| The scope an **approved card's** call runs on | The approving user — the only user who can claim that card, and the one whose turn staged it |
| A **webhook-started** turn (runs as the trigger owner, but nobody signed in sent it), evals, dataset runs | Keys **dropped** (unset) |
| **Every scheduled run** — dedicated `mcp_selection` client or shared, broker or in-process | Keys **dropped** (unset). A task scope that carried a user is refused by the broker before it opens. |
| Shared spawns — boot catalog, hot reload, load-on-demand onto a shared client, `fleet mcp test`, the transitional local-client mode | Keys **dropped** (unset): a process-lifetime subprocess serves every user |

**Whose identity in a shared chat.** Turns are attributed to the authenticated
user who sent them. Fleet conversations have exactly one owner and every turn
path loads the conversation owner-scoped, so the sender and the owner are the
same person; a teammate who can read a team-shared chat cannot send a turn in it
— they branch it into a chat of their own, where they are the sender
(`TEAM-SHARING.md`). Nothing reads the identity from the conversation row, the
transcript, the model or the bundle.

**Dropped, never blank.** A key whose value needs a token with no value is
removed from the subprocess env, the same posture as `${FLEET_TASK_ID}` on a
shared spawn: the server sees the variable as unset — never a literal token,
never an empty string, never a half-substituted `"Hi ${FLEET_USER_NAME},"`.
Each token is judged alone, so `${FLEET_USER_EMAIL}` still resolves for a user
who has not saved a first name. Bundles must keep their own "no user" behaviour (for the
deal sheet: "Hi team," and no default recipient), because every scheduled run
of the same connector takes it.

**Data, not instructions — and not authorization.** The values are validated
on both sides of the broker boundary against a conservative character set: an
email is one `@`, a local part of ASCII letters, digits and `. _ % + - '`, and
a dotted domain; a name is 1–64 runes of letters (with any combining marks that follow a letter), single spaces and `. ' -`. A
value that fails is treated as absent. Neither set admits `$`, `{` or `}`, so a
substituted value can never spell another reserved token. The user selects no
server, account seat or tool and is not part of the scope's authorization
policy (ADR-0042 is unchanged). A server must never use these variables as a
credential, a seat selector, or an access decision — they say who is at the
keyboard, nothing more. They are PII: fleet adds no log line that prints them,
and a bundle server should not log its env. For the same reason a key listed in
`identity_env` may not reference them: the load fails, since a key that routes
a seat or money must never be filled from whoever is at the keyboard.

**Named account seats.** The user is written into the env last, at spawn —
after the `<VAR>_<ACCOUNT>` overlay and its inherited-identity guard — so a
named seat (`deals_client_a`) gets the signed-in user exactly like the default
seat. An operator who sets an explicit `<VAR>_<ACCOUNT>` value for a
token-bearing key (`DEAL_SHEET_DEFAULT_TO_CLIENT_A`) overrides the token for
that seat, like any other account overlay.

The spelling rules are the other reserved tokens': bare only (a
`${FLEET_USER_EMAIL:-team@example.com}` default fails the load — it would bake
one person in), exact (a padded `${ FLEET_USER_EMAIL }` fails the load for
every reserved token, since the spawn paths match only the exact spelling and
it would otherwise reach every connector as a literal), not escapable (an
escaped `$${FLEET_USER_EMAIL}` fails the load anywhere in the manifest,
including inside a `${OUTER:-...}` default body: the escape collapses to — or
survives as a `$` in front of — the token before spawn, so it would deliver
the user's email rather than the literal text; there is no way to write that
literal), never resolved from the process env (an operator-exported
`FLEET_USER_EMAIL` cannot hijack it), and only in a **stdio** `mcp_servers` env
value (a header, url, command or an http server's env fails the load, since
nothing would ever substitute it there). An Agent Plugin's `mcp.json` env gets
the same escaped/padded refusal (that entry is skipped). As a spawn-time
backstop, a key whose value still carries an escaped or non-exact spelling is
dropped, never substituted or passed through. `fleet validate-config` and
bundle preflight accept the tokens exactly where they work. A bundle that never writes them is unaffected. Adopt them only
after the fleet release that ships them: an older fleet treats them as ordinary
unset variables and hands the server an **empty** value. The decision record is
[ADR-0087](adr/0087-interactive-mcp-scopes-carry-the-signed-in-user.md).

## The subprocess working directory

A stdio server's cwd is **not** the bundle root. Bundle-relative script args
(`mcp/foo.py`) are absolutized when the bundle loads, which frees the cwd from
having to be the bundle root, and each spawn path then launches the subprocess
in the same fleet-managed directory it substitutes for `${FLEET_WORKSPACE}`
(`agentcore.StdioCwd`).

This is not cosmetic. A server writes a relative output path — one the model
passed as `output_dir`, or one of its own defaults — against its cwd. While
that cwd was the bundle root, those writes landed in the operator's git
checkout: invisible to the agent (the sandbox never mounts the bundle
writable), untouched by reclamation (which sweeps the data dir), and
accumulating customer data inside a git repo. Two shipped connectors
additionally allowlist `os.getcwd()` as a readable root for email attachments,
which made the whole checkout an attachable source.

Two cases keep the bundle root (or the plugin root):

- **Agent Plugins.** The subprocess launches in its plugin root — a spec
  contract (ADR-0054), since its args are opaque strings fleet may not rewrite
  and it resolves its own bundled files against that root.
- **No workspace on offer, or one that does not exist on disk.** `exec` refuses
  to start a process whose cwd is missing, so an un-materialized path falls back
  rather than taking the server down.

Passing an **absolute** `output_dir` remains the rule for a connector tool: only
the interactive path's cwd is the agent's own workspace, so a relative path is
still not reliably readable from a scheduled run.

### 2. `MCP_VARIANT_CLIENT` injected on account-variant spawns

The cutlass `mcp_load_servers(client=…)` convention injects
`MCP_VARIANT_CLIENT=<client>` into every client-variant subprocess; the
servers read it to require variant-scoped identity config and to derive
client-facing labels (SSP fee-partner / fee-recipient names — revenue
routing). fleet's account-variant spawn (`agentcore.resolveMCPVariant`, used
by both the scheduled `mcp_selection` binder and `mcp_load_servers`) now does
the same: named-account stdio variants get
`MCP_VARIANT_CLIENT=<lowercased canonical account>`; the default seat never
carries it.

### 3. `identity_env` — the inherited-routing-identity guard

New optional per-server manifest field (stdio only):

```yaml
mcp_servers:
  - name: pubmatic_mcp
    ...
    identity_env: ["PUBMATIC_OWNER_ID"]
```

It names the env keys that route identity or money (owner/member/marketplace
ids, seat-routing tokens). A named-account spawn is REFUSED when any listed
key has a non-empty default-seat value that the account's `<VAR>_<ACCOUNT>`
overlay did not override — suffixing the API key but not the owner id would
otherwise transact in the DEFAULT client's seat under the named account's
label. This mirrors cutlass's `IdentityRoutingEnvVars` guard, but the list
lives in the bundle (fleet ships none). Validation is fail-loud at load: an
entry must name a key of that server's env map, and the field is rejected on
http servers. Listed names are also registered for the `.env` allowlist so
their suffixed forms survive the env-file load.

### 3a. Base vars must not be underscore-prefixes of siblings (#1124)

The `<VAR>_<ACCOUNT>` account convention is purely lexical, so a stdio server
that declares both `FOO` and `FOO_BAR` (as env keys or `account_vars`) is
ambiguous: selecting account `bar` would silently overlay `FOO` with
`FOO_BAR`'s value — a *different* credential injected under the wrong key —
and the account catalog would report `bar` as a phantom account of `FOO`
(e.g. `SLACK_TOKEN_URL` surfacing `url` as an "account" of `SLACK_TOKEN`).
Bundle validation now rejects such a pair at load, per server, with rename
instructions (`validateAccountSuffixBases`). Residual, deliberately not
enforced: the same pair split across two servers (or a base var colliding with
an unrelated process-env var) can still cross-wire, because the process env is
one namespace — bundle authors should keep credential var families
prefix-free bundle-wide.

### 4. Interactive approval staging for bundle-declared critical tools

`agent_policy.critical_tools` suffixes were enforced only in scheduled mode
(the confirm_audit gate). Interactively, only email/bash/preview/schedule had
gates — a suffix a bundle marked critical (deal creation/mutation, page
deploys, …) executed un-staged in chat, and `critical_tool_timeouts` was dead
for those tools. `InteractivePolicy.BeforeToolCall` now stages critical-suffix
tools through the SAME approval-card UX (`checkCriticalToolApproval`),
honoring the session pre-approval/pre-denial sentinels (#300) and the #225
per-tool timeout chain. Exemptions keep single ownership: outbound-email tools
stay with the email gate; bash/preview_email/schedule_task/
suggest_advanced_model keep their dedicated gates. Inert without an approval
sink (mirrors the risky-bash gate). Approval resolution already dispatches any
`mcp_<server>_<tool>` name (`CallToolPrefixed`), so no UI/API change was
needed.

### 5. Bundle-load vs `.env` ordering + unresolved-`${VAR}` fail-loud (fleet#1123)

`clientconfig.Load` folds the `FLEET_ENV_FILE` env file into the process env
BEFORE the manifest interpolation pass, via `config.BootstrapEnvFile` — the
same admission (allowlist-gated; every `${...}` reference the raw manifest
carries is registered first) and precedence (process env wins) as
`config.Load`, whose own later application is an idempotent re-read. Every
interpolated field — server `url`s, `command`/`args`, `sandbox.image`,
providers, branding — therefore resolves against the same environment the
runtime uses, and `${VAR:?...}` validates post-`.env`. The env-file PATH comes
only from the process env, never from the bundle, so the ordering has no
circularity. Three deliberate edges:

- MCP `env`/`headers` values and inline `http_tools` headers keep the
  fleet#706 lazy contract unchanged: raw `${...}` text through the load,
  resolved at catalog-build/spawn time against the live process env, where an
  unset credential is legitimate (gate off / `optional_env` drop).
- Outside those maps, an unresolved bare `${VAR}` now FAILS the load with an
  error naming the manifest field and the variable — nothing ever re-resolves
  a `url:`/`command:`/`sandbox.image`, so the literal token could never work.
  `${VAR:-default}` with the var unset still resolves to the default quietly;
  `$${...}` still writes an intentional literal. A bare reserved token
  (`${FLEET_WORKSPACE}`/`${FLEET_WORKSPACE_ROOT}`/`${FLEET_TASK_ID}`/`${FLEET_USER_EMAIL}`/`${FLEET_USER_NAME}`) outside an `mcp_servers` env value
  fails the same way — the spawn paths substitute it only there, so even in
  the lazily-RESOLVED header maps (where `interpolate()` merely preserves it
  verbatim) it would ship on the wire as the literal token. Likewise a
  `${VAR:-default}` outside the lazy maps whose default body nests an
  unescaped `${...}`: the interpolator never expands a default body, so the
  inner reference would ship as a literal whenever the outer var is unset
  (and its name never reaches the `.env` allowlist) — rejected as a spelling,
  set or unset; write `$${...}` inside the default if the literal is
  intended.
- `fleet validate-config`'s credentials check tells the two absence classes
  apart: a name whose EVERY manifest occurrence carries a `${VAR:-default}`
  (`EnvVarNamesDefaultOnly`) is reported as "manifest defaults in effect",
  not as a missing credential — a pristine generic-bundle install stays OK
  instead of permanently warning about `"${FLEET_SANDBOX_IMAGE:-}"`.
- The application is once per process, not per bundle load: the MCP broker
  child's catalog reload keeps its boot env snapshot (credential rotation
  still requires a restart), and the post-broker-boot parent scrub is never
  undone by a later bundle re-load.

`EnvVarNames()` correspondingly inventories `${VAR}` references from ALL
interpolated fields (captured from the raw manifest, so an already-exported
value still surfaces its name), not only env/header values — an env-file value
for a `url:`-referenced var survives the `.env` allowlist.

## What was deliberately NOT done

- **Per-conversation workdirs for shared MCP subprocesses** — impossible
  without per-conversation subprocess spawning; out of scope (see the honest
  scope note above).
- **Per-run workdirs for scheduled runs that use the shared client** (no
  `mcp_selection`): they get the shared per-deployment dir. Managed-run
  detection is still armed; only ledger granularity differs.

  **A shared dir is not a run identity — do not treat it as one.** Its ledgers
  accumulate the markers of every task *and* every chat conversation on the box,
  and the bundle's ledger records carry no run attribution. Two consequences,
  both now handled:

  - **fleet side.** `bindTaskMCP` returns **no** reconciliation workdir on the
    selection-less path, so the #717 start-of-run create-reconciliation sweep is
    never seeded from the shared ledger. Replaying it injected another task's
    half-finished creates into this task's prompt, and because an abandoned
    `submitted` marker is only cleared by a matching resolution in the same
    file, it was replayed into every subsequent run forever. When the catalog
    references `${FLEET_TASK_ID}` the run logs that its per-task ledgers are
    inert, so the operator knows what a missing selection costs.
  - **Bundle side.** A bundle ledger MUST key a per-task invariant to
    `${FLEET_TASK_ID}` (fleet's task UUID) and stay **inert** without it — never
    to the workdir path. elcano-config PR #48 fixes exactly this in the SendGrid
    send-once guard: keying on the shared path collapsed every task onto one
    key, so the first send on the box made every later send return
    `202 duplicate_suppressed` with no email delivered.

  A task that needs real per-run ledger semantics (cross-run send-once, create
  idempotency across a retry) declares an `mcp_selection` and gets the dedicated
  per-run client, whose workdir and `${FLEET_TASK_ID}` identify exactly one run.
  Making that the default for *every* scheduled run would mean a per-run spawn
  of the whole catalog and would have to reproduce the shared client's
  optional-server gating; it stays deferred.
- **A name from the identity provider.** Fleet's sessions carry an email only
  (password, OIDC and the central auth cookie alike), and the Elcano Auth
  service stores no name, so `${FLEET_USER_NAME}` is the first name the user
  saves in Settings. If Auth gains a `given_name` claim, the OIDC callback can
  seed `users.given_name` for users who have not saved one; nothing changes
  for bundles.
- **A user identity for webhook-started turns or scheduled runs.** Nobody
  signed in sent them. A task created from chat does not inherit its creator's
  identity into its runs either: a run may fire long after, on a schedule the
  creator no longer watches, and the scheduled path takes no user at all.
- **`${FLEET_USER_*}` on HTTP servers or in headers.** Only stdio `env`
  values are substituted, like the workspace tokens.
- ~~**Full re-ordering of bundle-load vs `.env` load**~~ — no longer a
  residual: fleet#1123 shipped the re-ordering plus a fail-loud check for
  unresolvable references. See "Bundle-load vs `.env` ordering" above.

## Cross-references

- Env interpolation semantics: `internal/clientconfig/clientconfig.go`
  (`interpolateManifest`, `resolveEnvMap`, `reservedWorkspaceVar`).
- Load-order + unresolved-reference check (fleet#1123):
  `internal/clientconfig/manifest_env.go` (`applyBootEnvFile`,
  `scanManifestEnvRefs`, `unresolvedManifestRefs`) +
  `internal/config/config.go` (`BootstrapEnvFile`).
- Spawn-time substitution: `internal/agentcore/mcp_workspace.go`; the user
  tokens and their validation:
  `internal/agentcore/mcp_user.go` (`ExpandUserEnv`, `NewMCPUserIdentity`).
- The saved first name: `internal/store/users.go` (`SetOwnGivenName`,
  `UserGivenName`, migration `081_users_given_name.sql`),
  `internal/httpapi/me.go` (`handleMyName`, `mcpUserFor`) and
  `web/src/app/settings/FirstNameRow.tsx`.
- Where the signed-in user comes from and how it crosses the broker:
  `internal/httpapi/chat.go` (`runTurnAsync`'s `mcpUser`),
  `internal/httpapi/approvals.go` (`approvalMCPScope`),
  `internal/mcpbroker/protocol.go` (`ScopeSpec.User`) +
  `server.go` (`validateScopeSpec`), `cmd/fleet/mcp_broker.go` (`OpenScope`).
- Variant guard + marker: `internal/agentcore/mcp_selection.go`.
- Interactive critical gate: `internal/agentcore/interactive_gates.go`
  (`checkCriticalToolApproval`) + `docs/AGENT-RUNTIME.md` (approval timeouts).
