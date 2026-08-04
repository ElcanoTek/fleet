# MCP bundle env contract: `${FLEET_WORKSPACE}`, `MCP_VARIANT_CLIENT`, `identity_env`, interactive critical-tool staging

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
operator's static process env.

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
| A scheduled task with an explicit `mcp_selection` (dedicated per-run client) | a fresh `<workspace-root>/mcp-runs/task-<id>-*` dir per run |

`<workspace-root>` is `FLEET_WORKSPACE_ROOT` (legacy `CHAT_`/`CUTLASS_`
aliases honored), else `./workspace`. A spawn path with no directory to offer
drops the token-bearing keys so the server sees the var as **unset** (its
documented inert posture) — never a literal token, never an empty string.

**Honest scope.** MCP subprocesses are shared across conversations (host-side,
process-lifetime), so the shared substitution is a *per-deployment* directory:
managed-run detection is on for everything the server process spawns, and
run-ledger entries persist across runs (a dedupe window, not a per-run
ledger). True per-run ledger semantics exist only on the dedicated-client path
(scheduled `mcp_selection` runs). Per-run dirs are deliberately not deleted at
run end — the ledger is post-run evidence. Bundles choose per server whether
to opt in at all: a server whose entry never references the token is
completely unaffected.

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
- **Full re-ordering of bundle-load vs `.env` load**: manifest interpolation
  still runs before the `.env` file is applied. MCP `env`/`headers` values and
  inline `http_tools` headers keep their raw `${...}` text through the load and
  resolve at catalog-build time against the live process env (after
  `config.Load` applies the `.env` file), so an env-file override of a
  `${VAR:-default}` key wins there — the fleet#706 residual. Every other
  manifest field (server `url`s, branding, providers, …) still resolves at
  load, before the `.env` file, and `${VAR:?...}` still validates against the
  pre-`.env` process env even inside connector values.

## Cross-references

- Env interpolation semantics: `internal/clientconfig/clientconfig.go`
  (`interpolateManifest`, `resolveEnvMap`, `reservedWorkspaceVar`).
- Spawn-time substitution: `internal/agentcore/mcp_workspace.go`.
- Variant guard + marker: `internal/agentcore/mcp_selection.go`.
- Interactive critical gate: `internal/agentcore/interactive_gates.go`
  (`checkCriticalToolApproval`) + `docs/AGENT-RUNTIME.md` (approval timeouts).
