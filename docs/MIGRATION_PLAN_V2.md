# Fleet Migration Plan v2 — One process, one runtime, one frontend on a single host

**Author:** Lead Architect · **Date:** 2026-06-21 · **Status:** SHIPPED — this migration is COMPLETE (historical plan; the code is canonical)
**Module:** `github.com/ElcanoTek/fleet` (single Go module) · **Target:** one `fleet` process on a single host

> **Reading note (post-migration).** This is the historical migration *plan*,
> retained for architectural rationale, not a live to-do list. The migration has
> shipped. Where any forward-looking note here (e.g. §1's control-plane claim, or
> §9 "Remaining open decisions") disagrees with the code, **the code is
> canonical.** For current architecture start from the README and `AGENTS.md`.

This plan carries v1's verified facts forward and rewrites every place where the nine LOCKED decisions + the five authoritative analyses change the design.

---

## 1. What changed from v1

- **No remote runners. gig is deleted as a repo.** v1 kept gig external (its own `go.mod`, runs on remote runners, talks HTTP). v2 folds gig **into** fleet as an in-process worker pool at `internal/runner`. (Correction vs as-shipped: the `/register` + `/nodes/*` routes were NOT fully removed — they survive as vestigial single-host lease plumbing backing ONE synthetic worker node; `/tasks/pending`, `/status`, `/logs` are gone. See `docs/openapi.yaml`.) Everything runs in the one `fleet` process.
- **ONE agent runtime, not two drivers behind seams.** v1 §6 kept `interactive/` and `scheduled/` as separate drivers sharing only primitives, and explicitly deferred "loop body in agentcore?" as open decision #13. v2 RESOLVES it: a **single run loop** `agentcore.Run(ctx, Mode, RunConfig, deps)`. Cutlass's outer enforcement loop is the base; chat is the 1-round special case (a `CanFinish`-always-true policy). `agent/` becomes ONE package.
- **ONE container execution model for both modes.** v1 preserved chat's Podman sandbox AND cutlass's direct `exec.CommandContext`, leaving "should cutlass containerize?" out of scope. v2 RESOLVES it: the **hybrid per-turn/per-exec-burst ephemeral container over a persistent per-session workspace** (chat's `sandbox.Pool`) is the SINGLE backend for both modes. Cutlass's direct-exec bash/python and **cutlass's runtime Containerfile are ELIMINATED**.
- **target_node_name is gone; replaced by per-task MCP + credential-account selection.** v1's orchestrator task form still carried `target_node_name`. v2 drops both node columns and adds a per-task MCP selection (`{server, account}` list) modeled directly on chat's per-conversation opt-in — the SAME mechanism for interactive and scheduled.
- **Multiple credential accounts per MCP, injected host-side only.** New in v2: an account store keyed by `(server, account)` over cutlass's `ApplyClientSuffix` `<VAR>_<ACCOUNT>` env convention. Credentials are injected into MCP **subprocesses** via `cmd.Env`, never into the sandbox container and never on argv.
- **Global concurrency cap.** New in v2: a single configurable `FLEET_MAX_CONCURRENT_AGENTS` semaphore bounds simultaneous agents across interactive + scheduled, sized `>=` warm-pool size.
- **ONE frontend, one toolchain.** v1 already chose one Next.js app; v2 hardens it: drop moc's jest/marked/DOMPurify/highlight.js/chart.js/eslint-10/vendoring-shim entirely; standardize on chat's vitest/react-markdown/eslint-9; the MCP picker is ONE shared component used in both the chat conversation toolbar and the orchestrator task form.
- **Postgres LOCKED to one cluster, two databases** (`chat` + `sched`). v1's two-schema fallback is dropped from consideration.
- **Both orchestrator login paths KEPT** (elcano_auth cookie + moc username/password Bearer). v1 open decision #8 ("drop moc password login?") is RESOLVED: keep both.
- **cutlass one-shot CLI demoted.** v1 kept `cmd/cutlass` as the gig-invoked container entrypoint. With no remote runners and no per-task container launch, the scheduled path is an **in-process worker call**, not a CLI spawn. `cmd/cutlass` survives only as an optional local debug entrypoint, not the production scheduled path.

---

## 2. Revised target architecture

One `fleet` binary on a single host. It exposes two HTTP listeners (chat-server :8080, orchestrator :8000) behind one Next origin, runs one scheduler ticker + one capped in-process worker pool, and drives BOTH interactive turns and scheduled tasks through ONE unified agent runtime over ONE ephemeral-container sandbox pool. Credentials live host-side in MCP subprocesses; the sandbox is credential-free. Only `lifeline` (external per-dev tool) and the agent-emitted sandbox containers live outside the process.

```
                    Browser  (one Next.js origin; elcano_auth cookie OR moc bearer)
                        │
   ┌────────────────────┼───────────────────────────────────────────────────────────┐
   │  ONE fleet process (single host)                                                     │
   │                    │                                                              │
   │   ┌────────────────▼─────────────────┐    ┌───────────────────────────────────┐  │
   │   │ HTTP/SSE servers                  │    │ Scheduler ticker (30s)            │  │
   │   │  • chat-server :8080  (SSE turns) │    │  promote scheduled→pending        │  │
   │   │  • orchestrator :8000 (REST)      │    │  RecoverExpiredLeases (crash-safe)│  │
   │   │  • dual auth: elcano_auth + bearer│    └──────────────┬────────────────────┘  │
   │   └───────┬───────────────────────────┘                   │ leases pending tasks │
   │           │ interactive turn                                ▼                      │
   │           │                          ┌──────────────────────────────────────────┐ │
   │           │                          │ Capped worker pool  internal/runner       │ │
   │           │                          │  sem = FLEET_MAX_CONCURRENT_AGENTS (cap)  │ │
   │           │                          │  ClaimNextPendingTask (FOR UPDATE SKIP    │ │
   │           │                          │   LOCKED) + lease-renew ticker            │ │
   │           │                          └──────────────┬───────────────────────────┘ │
   │           │  Mode=Interactive                        │  Mode=Scheduled             │
   │           └──────────────┬───────────────────────────┘                            │
   │                          ▼                                                          │
   │       ┌───────────────────────────────────────────────────────────────┐           │
   │       │ UNIFIED agent runtime   internal/agentcore                      │           │
   │       │  Run(ctx, Mode, RunConfig, deps) — ONE outer enforcement loop   │           │
   │       │  (chat = 1-round special case via CanFinish-always-true)        │           │
   │       │  seams: InputSource · Observer · Policy · Executor              │           │
   │       └───┬──────────────────────┬─────────────────────────┬───────────┘           │
   │           │ Executor             │ MCP wiring              │ DB                      │
   │           ▼                      ▼                         ▼                         │
   │  ┌─────────────────┐   ┌────────────────────────┐  ┌────────────────────────────┐   │
   │  │ Sandbox pool    │   │ MCP credential-account │  │ Two DB pools (one cluster) │   │
   │  │ sandbox.Pool    │   │ store (host-side)      │  │  • chat  *sql.DB (pgx)     │   │
   │  │ warm Size<=cap  │   │  ApplyClientSuffix     │  │  • sched *sql.DB (pgx)     │   │
   │  │ per-turn/burst  │   │  <VAR>_<ACCOUNT> →      │  └────────────────────────────┘   │
   │  │ ephemeral ctr   │   │  cmd.Env (never argv)  │                                     │
   │  └───────┬─────────┘   └───────────┬────────────┘                                    │
   └──────────┼───────────────────────┼─────────────────────────────────────────────────┘
              │ podman run (rootless)  │ stdio subprocess  (host-side, credentialed)
              ▼                        ▼                            ▼ external HTTP MCP
   ┌─────────────────────┐  ┌────────────────────────┐   ┌────────────────────────┐
   │ EXTERNAL sandbox     │  │ MCP server subprocesses │   │ lifeline MCP (external,│
   │ containers (ephem.)  │  │  <server> / <server>_   │   │  per-dev tool — NOT in │
   │  bash/python over a   │  │  <account>  CREDENTIALED │   │  the fleet module)     │
   │  persistent workspace │  │  (host-side, reaped at  │   └────────────────────────┘
   │  bind  CREDENTIAL-FREE │  │   run end)              │
   └─────────────────────┘  └────────────────────────┘
```

Key invariants the diagram encodes:
- **Sandbox is credential-free.** `ContainerConfig` has no `Env` field (confirmed: only `ExtraRunArgs` at container.go:117) and must not gain one. Credentials only ever reach MCP subprocesses host-side.
- **Cap and warm pool are orthogonal and both exist.** Warm pool = pre-spawned ready containers (Size). Global cap = max live containers across both modes (semaphore). Acquire the semaphore slot **before** `pool.Take()`; release **after** cleanup. `warmSize <= cap`.
- **One run loop.** The Mode enum + RunConfig + four seams (InputSource, Observer, Policy, Executor) are the only divergence axes; the loop body is shared. The genuine Mode branches are chat's leaked-tool-call / force-final-summary recovery (interactive only) and the Executor backend choice (both behind the one `Executor` interface).
- **Crash recovery = throw the box away.** Durable state lives in the bind-mounted workspace volume and in Postgres leases; a dead container is replaced by a cold-start against the same workspace, and a dead fleet process re-claims its in-flight task when its lease expires.

---

## 3. Revised directory layout

```
/root/fleet/
├── go.mod                          # module github.com/ElcanoTek/fleet, go 1.26.4
├── go.sum
├── Makefile
│
├── cmd/
│   ├── fleet/                      # THE fleet binary: chat HTTP/SSE + orchestrator HTTP
│   │   └── main.go                 #   + scheduler ticker + capped in-process worker pool
│   ├── fleet-admin/                # unified CLI: bootstrap, chat/sched users, MCP accounts
│   │   └── main.go
│   ├── cutlass/                    # OPTIONAL local one-shot debug entrypoint (NOT prod path)
│   │   └── main.go
│   └── sandbox-probe/              # deploy-time smoke: Take + a scheduled-agent task
│       └── main.go
│
├── internal/
│   ├── agentcore/                  # ★ ONE unified run loop + shared primitives
│   │                               #   Run(ctx,Mode,RunConfig,deps); provider/model,
│   │                               #   MCP-wrap/buildFantasyTools, cache, resilience,
│   │                               #   openrouterCost, checkRepeatedCall, fast.io guard,
│   │                               #   MCPSelection/MCPChoice type, ApplyClientSuffix
│   ├── agent/                      # ★ ONE package (NO interactive/+scheduled/ split):
│   │                               #   input sources (live SSE TurnInput | one-shot task),
│   │                               #   observers (SSE turn_buffer | captain's-log JSON),
│   │                               #   policies (interactive bundle | scheduled bundle),
│   │                               #   finalize() with the Mode-keyed recovery branch,
│   │                               #   verifier.go (scheduled-only), session_log.go
│   ├── runner/                     # ★ gig folded in: in-process capped worker pool
│   │                               #   pool.go (global semaphore + claim loop + taskWG drain)
│   │                               #   claim.go (ClaimNextPendingTask, FOR UPDATE SKIP LOCKED)
│   │                               #   run.go (in-process status/log writes, lease-renew ticker)
│   │                               #   (gig.runContainer logic absorbed by the Executor seam)
│   ├── mcp/                        # ONE merged Go MCP client (stdio+HTTP); credential
│   │                               #   injection on cmd.Env in NewStdioTransport
│   ├── sandbox/                    # ONE execution backend (chat's): container.go, host.go
│   │                               #   (test fixture), pool.go, bridge. The SINGLE Executor.
│   ├── tools/                      # native tools; ONE bash/python over sandbox.Sandbox
│   │                               #   (cutlass direct-exec deleted; WaitDelay + 64MB cap
│   │                               #    knobs folded into the unified bash path)
│   ├── store/                      # chat (interactive) Postgres layer + migrations
│   │   └── migrations/
│   ├── sched/                      # orchestrator (was moc): handlers, scheduler ticker,
│   │   │                           #   storage, models, apikeys
│   │   └── db/
│   │       └── migrations/         #   001..013 + 014_replace_target_node_with_mcp_selection
│   ├── httpapi/                    # chat HTTP/SSE/auth layer
│   ├── creds/                      # ★ MCP credential-ACCOUNT store: reads <VAR>/<VAR>_<ACCOUNT>
│   │                               #   from the 0600 env file; account catalog by suffix scan;
│   │                               #   ApplyClientSuffix-based per-account env overlay
│   └── config/                     # unified config (FLEET_ prefix, URL DSN builder,
│                                   #   centralized env loading shared by both modes)
│
├── web/                            # ONE Next.js 16 app (chat's toolchain)
│   ├── package.json                # next 16.2.9, react 19.2.7, tailwind 4, vitest, eslint 9
│   ├── middleware.ts               # ONE gate: /chat/* and /orchestrator/*; both login paths
│   ├── e2e/                        # ONE Playwright suite (chat e2e + ported moc frontend.spec)
│   └── src/app/
│       ├── chat/                   # View A — lifted verbatim
│       ├── orchestrator/           # View B — moc ES6 re-ported to React
│       ├── login/                  # ONE login card: password form + "Use Elcano email"
│       ├── shared/ui/              # McpServerPicker, ModelPicker, Toast, ConfirmDialog, FileUpload
│       ├── shared/hooks/           # useMcpServers, useOrchestratorSession, useDashboardData, ...
│       ├── shared/lib/             # auth, csrf, chatServer, mocServer, validation, format, cron
│       └── api/
│           ├── chat/*              # proxy → :8080 (X-Chat-Server-Token + X-User-Email)
│           ├── conversations/[id]/mcp-servers/route.ts   # chat opt-in (now carries account)
│           └── orchestrator/*      # proxy → :8000 (stats,nodes,tasks,logs,upload,config,
│                                   #   mcp-servers, mcp-accounts, concurrency)
│
├── images/sandbox/Containerfile    # ★ THE one sandbox image bash/python run in
│                                   #   (cutlass runtime Containerfile ELIMINATED)
├── deploy/                         # fleet.target, fleet-cli, Caddyfile (cutlass/ image gone)
├── scripts/                        # bootstrap.sh (--postgres=local|external), build-sandbox-image.sh
├── personas/  protocols/  system_prompts/  skills/   # stable abs paths for same-path :ro,z mounts
└── mcp/                            # ONE deduped set of Python MCP servers (§5)
    ├── email_lint.py  sendgrid_server.py  ses_s3_email.py
    ├── xandr_mcp.py  magnite_mcp.py  medianet_mcp.py  pubmatic_mcp.py
    ├── indexexchange_mcp.py  openx_mcp.py  triplelift_mcp.py  deal_sheet_server.py
    ├── gamma.py  mailbux.py          # chat-only, kept
    └── tests/
```

Structural deltas from v1's tree: `internal/agent/{interactive,scheduled}` collapses to one `internal/agent`; the run loop moves into `internal/agentcore`; `internal/runner` is new (gig folded in); `internal/creds` is new (MCP credential accounts); `internal/captainslog` collapses into `internal/agent`; `deploy/cutlass/Containerfile` is **deleted**; the sched migration set gains pair 014.

---

## 4. Source → target mapping deltas (only what changed vs v1)

| Area | v1 mapping | v2 mapping (override) |
|---|---|---|
| **gig (entire repo)** | Drop from fleet module; stays its own repo on remote runners | **Folded into `internal/runner`.** `sem`/`pollLoop`/`taskWG` (main.go:152,340,628,710) → pool global-cap semaphore + claim loop + graceful drain. `runContainer` (main.go:1062) → absorbed by the Executor seam (now `sandbox.Pool.Take`, not `podman run cutlass`). `register`/`heartbeatLoop`/`postJSON` (main.go:370,500) + the HTTP file-download path (main.go:899) → **deleted** (files are local; status/logs become direct storage calls). gig repo deleted. |
| **agent packages** | Two packages: `internal/agent/interactive` (chat session/finalize) + `internal/agent/scheduled` (cutlass Execute) as thin drivers over shared primitives | **ONE package `internal/agent`** + the run loop in `internal/agentcore`. chat `session.go::RunTurn` and cutlass `agent.go::Execute` both **deleted as separate loops** and reconstructed as Mode/RunConfig calls into `agentcore.Run`. cutlass's outer `for round < maxEnforcementRounds` (agent.go:1079) is the base loop; chat's single pass = `CanFinish`-always-true round-1 collapse. chat's leaked-call retry + `forceFinalSummary` (session.go:544, finalize.go:83) stay **only** in the Interactive `finalize()` branch. |
| **Frontend tooling** | One Next app; standardize on chat (eslint 9, vitest, react-markdown) — stated as recommendation | **Hard-locked.** DELETE moc's jest, marked, dompurify, highlight.js, chart.js, eslint 10, and the `install-vendor.cjs`/`verify-vendor.cjs` shim. ONE `web/package.json` = chat's. moc's 8 jest test files re-authored as vitest `.test.tsx`; moc `frontend.spec.ts` ported into chat's Playwright matrix. ONE `<McpServerPicker>` reused in chat toolbar (`mode="conversation"`) and orchestrator task form (`mode="task"`). |
| **Container / Executor** | Keep BOTH backends: chat Podman sandbox + cutlass direct-exec, behind a `CodeExecutor` seam; "containerize cutlass?" out of scope | **ONE backend.** chat's `sandbox.impl{runBash/runPython/close}` (sandbox.go:118) is THE Executor. The scheduled path's tools change from `NewBashTool()` direct-exec to `NewBashTool(sb)`. **DELETE** cutlass `internal/tools/bash.go` direct `exec.CommandContext` (bash.go:426), cutlass `python_repl.go` in-process bridge (python_repl.go:97), and cutlass `Containerfile`. **Port into the unified bash path before deleting:** cutlass's `WaitDelay` (bash.go:303) and 64MB `cappedBuffer` (bash.go:296). Hybrid lifecycle: per-turn (interactive) / per-exec-burst (scheduled) ephemeral container over a persistent same-path workspace subdir (container.go:330 `:z`). Recovery = Close + re-Take against the same workspace subdir. |
| **MCP-selection / credential-account system** | Did not exist; v1 carried moc's `target_node_name` for routing | **NEW + REPLACES `target_node_name`.** New `internal/creds` store over `ApplyClientSuffix` (config.go:999). New sched migration `014` DROPs `target_node_id`/`target_node_name`, ADDs `mcp_selection JSONB DEFAULT '[]'`. New model field `MCPSelection []MCPChoice{Server,Account}` on `Task`/`TaskCreate`; `NodeMatchesTask` glob path (storage.go:34) deleted. chat conversation row's `OptionalMCPServersEnabled` JSONB generalized to tolerate `{server,account}` objects. ONE `buildFantasyTools` gate (fantasy.go:291) feeds both modes. |
| **Concurrency cap** | None | **NEW.** `FLEET_MAX_CONCURRENT_AGENTS` buffered-channel semaphore in `internal/runner`, global across interactive + scheduled; `warmSize <= cap`; acquire before `pool.Take`, release after cleanup. |

Unchanged from v1 (carried forward verbatim): the chat→fleet import-path rewrite, single `go.mod`, `internal/mcp` Go-client merge (§5 ledger), `internal/store` + `internal/sched/db` co-located incompatible migrations, chat URL-form DSN builder as standard, lifeline external.

---

## 5. MCP dedup ledger (carried forward from v1, unchanged)

moc contributes zero MCP code; this is entirely chat-vs-cutlass. No analysis changed these decisions.

| Artifact | Decision | Winner / action |
|---|---|---|
| Go `internal/mcp/client.go` | **MERGE** | Base = chat; fold cutlass `HasServer` + `isRequestNotDeliveredError` + hoisted JSON-RPC constants + `matchesInt`; keep chat's per-call single-reader + server-scoped `CallToolOn` (sendgrid & mailbux both export `send_email`). |
| `agent/mcp_fastio_response.go` | **KEEP EITHER** | Byte-identical; keep one in `agentcore`. |
| `agent/mcp_fastio_guard.go` | **MERGE** | Parameterize the remediation-hint (blob flow vs native `fastio_upload_file`); expose both. |
| `email_lint.py` | **KEEP EITHER** | Byte-identical (1151 LOC). |
| `sendgrid_server.py` | **MERGE** | Combine chat's `_check_template_leakage`/`_read_file_content` + cutlass's `_validate_path_security`. |
| `ses_s3_email.py` | **KEEP CUTLASS** | Strict superset (adds `find_latest_report`). |
| `xandr_mcp.py` | **KEEP CUTLASS** | Newer + create/update deal ops; chat is downstream. |
| `magnite_mcp.py` | **KEEP CUTLASS** | 2.6× tool surface. |
| `medianet_mcp.py` | **KEEP CUTLASS** | Newer + richer; per-tool diff first (close counts). |
| `indexexchange_mcp.py` | **KEEP CUTLASS** | Newer + larger; reconcile 2-tool delta. |
| `pubmatic_mcp.py` | **MERGE** | Base = cutlass (prepared-deal); port chat's discovery tools (`pm_discover_dsps` etc.). |
| `openx_mcp.py`, `triplelift_mcp.py`, `deal_sheet_server.py` | **KEEP CUTLASS** | cutlass-only. |
| `gamma.py`, `mailbux.py` | **KEEP CHAT** | chat-only. |

---

## 6. MCP selection, credential accounts & concurrency cap

This is ONE mechanism that both interactive conversations and scheduled tasks feed, modeled on chat's per-conversation opt-in. It REPLACES moc's `target_node_name` (meaningless on one box).

> **Implementation status (2026-06-23): per-account selection shipped for
> SCHEDULED tasks only.** The credential-account mechanism (`creds`,
> `ApplyClientSuffix`, `MCPSelection`/`MCPChoice`, the account refusal guard, the
> `server_account` variant subprocess) is live, and a scheduled task picks
> `{server, account}` via its `mcp_selection`. The INTERACTIVE chat side is still
> server **on/off only** — a conversation persists `OptionalMCPServersEnabled
> []string` (no per-conversation account), so every interactive turn runs the
> default seat. The §6.1/§6.2 claims below about the chat conversation row
> carrying `{server, account}` and `scanOptionalMCPServers` decoding both forms
> describe the DESIGN, not the current code; surfacing the account picker in the
> chat toolbar remains the deferred open decision in §9.5. This matches legacy
> chat behavior (no regression).

### 6.1 The unified selection shape

Defined in `internal/agentcore` (where `buildFantasyTools` lives):

```go
// MCPChoice = which optional server is on + which credential account backs it.
// Account=="" means the default/shared seat. This is chat's opt-in list
// (a []string of server names) with one string added per entry.
type MCPChoice struct {
    Server  string `json:"server"`            // catalog key, e.g. "xandr"
    Account string `json:"account,omitempty"` // e.g. "client_a"; "" = default
}
type MCPSelection []MCPChoice
```

Both producers reduce to the SAME two arguments chat already passes to the gate (`buildFantasyTools(..., optionalServers, optInSet)`, fantasy.go:235):
- **`optionalServers`** — the authoritative catalog of Optional servers (built once at startup). Unchanged. Always-on servers (sendgrid, email) contribute unconditionally.
- **the per-run enabled set** — derived from `MCPSelection`'s server names. Gate 1 (`if optionalServers[name] && !optIn[name] { skip }`, fantasy.go:291) is **byte-identical** for both modes. Accounts do **not** affect which tools register; they affect which subprocess/env backs the server (§6.3).

**Interactive producer (unchanged behavior):** the chat conversation row stores the selection (chat migration 003's `optional_mcp_servers_enabled` JSONB). Extend the JSONB so each element is either a bare string (back-compat) or a `{server,account}` object; `scanOptionalMCPServers` (store.go:181) tolerantly decodes both. The POST handler additionally validates `account` against the server's account catalog.

**Scheduled producer (new, mirrors it):** the task row stores the same shape (§6.2). Same gate, same wiring.

### 6.2 Schema (sched DB, migration 014)

```sql
-- 014_replace_target_node_with_mcp_selection.up.sql
ALTER TABLE tasks DROP COLUMN IF EXISTS target_node_name,
                  DROP COLUMN IF EXISTS target_node_id;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS mcp_selection JSONB NOT NULL DEFAULT '[]'::jsonb;
```
JSONB (not `TEXT[]`) to keep the `database/sql` `[]byte`+`json.Marshal` plumbing — mirrors chat's migration 003 rationale, avoids `lib/pq` pgtype, and matches the `pgx` convergence. Update `taskColumns`, `AddTask` INSERT/UPDATE, and `scanTask` (db.go:521+) to swap the two `target_node` columns for `mcp_selection`. Remove `target_node_name` from the three task schemas in `openapi.yaml`; add `mcp_selection`. Example row value:

```json
[{"server":"xandr","account":"client_a"},{"server":"magnite"}]
```

### 6.3 Selection → run-config → secure credential injection (shared path)

`agentcore` converts an `MCPSelection` into per-run MCP wiring the SAME way for both modes. For each chosen `{server, account}`:
1. Build the server's base env via the unified `ProviderMCPEnv(server)` / `EmailMCPEnv` builders (config.go:697).
2. `variantEnv, overrides := config.ApplyClientSuffix(baseEnv, account)` (config.go:999) — overlays `<VAR>_<ACCOUNT>` over `<VAR>`. **If `account != "" && overrides == 0`, REFUSE** (cutlass guard, mcp_loader.go:370) — never silently inject the default seat under an account label.
3. Spawn/select the MCP under name `server` (default) or `server_account` (variant), via `client.AddStdioServer(ctx, name, cmd, args, variantEnv)` → `NewStdioTransport` sets it on **`cmd.Env`** (client.go:538). **Credentials are never on argv and never enter the sandbox container.** HTTP (fast_io) servers reject account variants (mcp_loader.go:351).

This is byte-for-byte chat's existing host-side injection path; interactive and scheduled differ ONLY in who built the `MCPSelection` (conversation row vs task row).

**Per-run isolation / reaping:** for scheduled runs, spawn the task's account-variant subprocesses at run start and **Close them at run end** (deferred cleanup tied to the worker's run lifecycle, not just `Manager.Close`) so no credentialed subprocess leaks across runs. A crash between spawn and Close is the one leak risk — the deferred cleanup must run on the worker's recover path.

**Account store (`internal/creds`):** secrets stay at rest in the 0600 `.env.local` (matching how chat AND moc store recoverable secrets today — Postgres holds only non-recoverable bcrypt/sha256 hashes). The store is the set of `<VAR>` and `<VAR>_<ACCOUNT>` keys; the account catalog is derived by scanning suffixes (like chat's `collectGammaKeys` / cutlass's `_list_account_names`). CLI verbs (write-only, value via stdin, never argv):
```
fleet mcp account set <server> <account> --secret KEY=-
fleet mcp account list <server>          # names only, never values
fleet mcp account del <server> <account>
```
A read-only `GET /api/orchestrator/mcp-servers` returns the catalog (server, accounts[] names only — never secret values), mirroring chat's `GET /mcp-servers` so the task form reuses the SAME picker component.

### 6.4 Concurrency cap enforcement

A single configurable global cap bounds simultaneous agents across both modes, enforced in `internal/runner`:

```go
sem := make(chan struct{}, cfg.MaxConcurrentAgents)   // THE global cap
// per claimed task / per interactive turn:
sem <- struct{}{}                  // acquire BEFORE pool.Take (blocks at cap)
sb, err := pool.Take(ctx)          // warm pool refills opportunistically up to Size
... run ...
cleanup(); <-sem                   // release AFTER cleanup
```
- The semaphore is THE cap; the DB lease is the crash-safe backstop. `warmSize <= cap` (else parked-but-unusable warm containers get double-counted / deadlock). Acquire the semaphore **before** Take.
- Default `FLEET_MAX_CONCURRENT_AGENTS = 4`; validated like cutlass's iteration bound.
- Single-box invariant: the per-process semaphore is correct ONLY because there is one box; the DB lease enforces mutual exclusion per task, not a numeric cap. Documented as a one-box invariant.

---

## 7. Bootstrap & DB

Carried forward from v1, with the worker-pool and credential-store changes.

### 7.1 Carried forward (unchanged)
- **`--postgres=local|external` branch** consumed once at the top of `fleet bootstrap` (default `local`). Branch A (local) = dnf+initdb+pg_hba+systemctl+`\gexec` idempotent role/db, `sslmode=disable`. Branch B (external) = skip install, validate DSN with `SELECT 1`, assume pre-provisioned roles/dbs (opt-in superuser creation), `sslmode=require`.
- **ONE cluster, TWO databases** (`chat` + `sched`), two owner roles, two `DATABASE_URL`s — LOCKED. The single `fleet` process opens two independent `*sql.DB` pools (25/5/5m each). Converge both onto `pgx` (drops `lib/pq`).
- **Bootstrap never runs migrations** — each service self-migrates on first start (chat's advisory-lock runner; sched's golang-migrate).
- chat's URL-form DSN builder (`url.UserPassword`) is the standard; centralized in `internal/config`.

### 7.2 Changes from folding the worker pool in
- **No separate runner provisioning.** v1 documented gig's HTTP runner contract (`/register`, `/tasks/pending`, ...) as an external surface to deploy on remote machines. v2 deletes it: there is no runner to register, no node row to seed, no per-runner `GIG_MAX_CONCURRENT_TASKS`. The worker pool starts inside `cmd/fleet/main.go` alongside the scheduler ticker. Bootstrap drops any node/runner setup step.
- **Synthetic single-worker identity.** moc's lease/ownership machinery is preserved verbatim but points at one fixed in-box `lease_owner` (sentinel) instead of many node UUIDs. The lease columns (migration 007), `RecoverExpiredLeases` (db.go:1317), and `UpdateTaskStatusAtomic` lease-renew (storage.go:943) are unchanged; the worker runs its own lease-renew ticker (renew well inside the 5-minute window, e.g. every 1–2 min) since heartbeats are gone. Tag each claim with a per-claim lease token so a goroutine whose lease was recovered cannot clobber the new claim's state.

### 7.3 Credential-store bootstrap step
- New: bootstrap ensures the 0600 `.env.local` exists and is writable by the fleet service user (it already does for chat/moc secrets). No new secret backend, no DB-stored recoverable secrets, no KMS. MCP account secrets are added post-bootstrap via `fleet mcp account set` (§6.3). Bootstrap may emit a reminder listing servers that have an account catalog but no `<VAR>_<ACCOUNT>` keys set.

---

## 8. Revised phased migration sequence (P0–P8, test-gated)

Each phase ends with a checkpoint of **named, real existing test suites** that must be green before the next phase. The central strategy: **build the unified runtime directly and validate it with BOTH front-ends' existing suites as a parity oracle** — neither suite may regress.

### P0 — Module skeleton & casing cutover
**Moves:** `go.mod` (`github.com/ElcanoTek/fleet`, go 1.26.4), `/cmd` + `/internal` skeleton (incl. empty `agentcore`, `agent`, `runner`, `creds`), Makefile, golangci-lint v2 config.
**Checkpoint:** `go build ./...`; lint config loads. No app tests yet.

### P1 — `internal/mcp` (shared island) + `internal/creds`
**Moves:** merge chat+cutlass Go MCP clients (§5). Build `internal/creds` over `ApplyClientSuffix`.
**Checkpoint:** chat MCP client tests + cutlass `mcp_loader_test.go` (`TestLoadMCPServers_*`, `TestLoadMCPServersWith_ClientVariantSpawnsSeparateServer`) green; new creds tests assert `ApplyClientSuffix` override-count and the `overrides==0` refusal.

### P2 — `internal/agentcore` unified run loop
**Moves:** build ONE `Run(ctx, Mode, RunConfig, deps)` — cutlass's outer enforcement loop as base, chat as the 1-round `CanFinish`-true collapse. Move shared primitives (provider/model, `buildFantasyTools`, cache, resilience, `openrouterCost`, `checkRepeatedCall` with parameterized noun, fast.io guard) + `MCPSelection`/`ApplyClientSuffix`. Define the four seams (InputSource, Observer, Policy, Executor). Support in-loop tool rebuild between rounds (cutlass `mcpServersDirty`).
**Checkpoint (parity oracle, both must stay green):** lift chat `cache_test.go`, cutlass `resilience_test.go`/`orchestration_test.go`, byte-identical `mcp_fastio_response_test.go`. New unified tests: `TestInteractivePolicy_CanFinish_AlwaysRound1` (the 1-round collapse), `TestRunConfig_MCPBinding_DefaultAccount`/`_NamedAccount` (ApplyClientSuffix survives the merge).

### P3 — ONE `internal/agent` package + ONE sandbox Executor
**Moves:** reconstruct interactive + scheduled behavior as Mode/RunConfig calls into `agentcore.Run` in a SINGLE `internal/agent` package (InputSource/Observer/Policy impls; finalize() with the Mode-keyed recovery branch; verifier scheduled-only). Move chat's `internal/sandbox` as THE Executor. Merge `internal/tools`; change scheduled bash/python to `NewBashTool(sb)`; fold in cutlass `WaitDelay` + 64MB cap; standardize on chat's stricter denylist. **Delete** cutlass direct-exec bash/python and cutlass `Containerfile`.
**Checkpoint (full parity oracle):**
- Interactive mode against unified loop: chat `session_test.go`, `finalize_test.go`, `overflow_test.go`, `orchestration_test.go`, `prompt_test.go`, `roster_test.go`, `mcp_optin_test.go` (`TestBuildFantasyTools_OptionalServer_Dropped/Passes`), `native_optin_test.go`, `image_input_test.go`.
- Scheduled mode against unified loop: cutlass `agent_test.go`, `execute_test.go`, `execute_integration_test.go`, `orchestration_test.go`, `verifier_test.go`, `compaction_integration_test.go`, `resilience_test.go`, `session_log_test.go`, `toolresult_test.go`.
- Sandbox (the Executor contract): chat `sandbox_test.go` (`TestPoolContainerMode`, `TestPoolContainerFailureSurfacesToCaller`, `TestContainerPythonBridge`, the cross-conversation-leak regression at :342), `workspace_same_path_test.go` (`TestContainerWorkspaceSamePathCoherence`, `TestContainerWorkspaceSurvivesConcurrentMount`, `TestContainerReadOnlyMountsSamePath`, `TestContainerBashSamePath`), `sandbox_hardened_test.go` (opt-in `CHAT_SANDBOX_HARDENED_TEST=1`).
- New: scheduled-driver-over-sandbox port of cutlass `tools_integration_test.go` asserting identical bash/python results through `sb.RunBash`/`sb.RunPython`; ported cutlass bash `WaitDelay` + `cappedBuffer` tests on the unified path.

### P4 — Python MCP servers deduped
**Moves:** consolidate `/mcp` per §5 (cutlass DSP servers win; pubmatic + sendgrid merge; keep gamma/mailbux; one email_lint). Wire the unified runtime to the single `/mcp` dir.
**Checkpoint:** consolidated pytest (`test_sendgrid_*`, `test_ses_s3_email`, `test_xandr_reporting`, `test_pubmatic_*`, `test_medianet_*`, `test_triplelift_reporting`, `test_mcp_integration`) green; `@pytest.mark.expensive` skipped; per-tool diff verification for medianet/indexexchange.

### P5 — `internal/runner` (gig folded in) + MCP-selection/credential/cap system
**Moves:** build `internal/runner` (global-cap semaphore + `ClaimNextPendingTask` FOR UPDATE SKIP LOCKED + lease-renew ticker + taskWG drain) absorbing gig's `sem`/`pollLoop`/`runContainer` logic; **delete the gig repo**. Move moc `scheduler`/`storage`/`handlers`/`models`/`apikeys`/`db` into `internal/sched/*`; converge to `pgx`. Add sched migration 014 (drop target_node, add `mcp_selection`), model field, generalized chat opt-in JSONB. Wire `FLEET_MAX_CONCURRENT_AGENTS`.
**Checkpoint:**
- Crash-recovery substrate (most critical): moc `leasing_test.go` (`TestTaskLeasing`, `TestRecoveredTaskRejectsOldNode` adapted to the synthetic worker, `TestTaskLeasingUsesFixedLeaseWindow`); moc `storage_test.go` (`TestRecurringTaskRescheduling`, `TestUpdateNodeHeartbeatRenewsActiveTaskLease` → in-process renew, `TestUpdateTaskStatusAtomicIgnoresStaleRunningAfterSuccess`); moc `db_test.go`.
- New: `TestClaimNextPendingTask` (SKIP LOCKED claims exactly one; two concurrent claims never double-lease); cap-saturation test (cap=N, N+1 tasks → exactly N concurrent, extra stays pending); systemd-restart-mid-task test (claim+lease, kill worker, advance clock past lease, `RecoverExpiredLeases`, assert re-queued + re-claimable); per-task selection test (`mcp_selection=[{xandr,client_a}]` → only that server with that account's creds); graceful-drain (SIGTERM → taskWG waits, terminal status + log via background ctx).
- Retire moc `handlers_test.go` `TestStatusReporting`/`TestLogSubmission`/register/heartbeat tests (endpoints deleted; assertions migrate into storage-level tests).
- Run moc tests `-p 1 -race` against a test Postgres.

### P6 — Scheduler + servers into the one fleet process
**Moves:** build `cmd/fleet/main.go` booting chat HTTP/SSE + orchestrator HTTP + scheduler ticker + the capped worker pool in ONE process; `cmd/fleet-admin` unified CLI (incl. `mcp account` verbs); `cmd/cutlass` demoted to optional debug.
**Checkpoint:** chat `httpapi` tests; moc `handlers_test.go` (surviving), `visibility_test.go`, `models_test.go`; scheduler promotes scheduled→pending and the in-process pool leases+runs it; `sandbox-probe` exercises both `Pool.Take` AND a scheduled-agent task.

### P7 — Unified frontend (one toolchain, two views)
**Moves:** scaffold `/web` from chat; lift chat under `/chat`; re-port moc dashboard to React `/orchestrator` (replace the `target_node_name` input with `<McpServerPicker mode="task">` + account dropdowns); add `mocServer.ts` + `/api/orchestrator/*`; ONE `middleware.ts` gating both segments and accepting both login paths; delete moc's jest/marked/dompurify/highlight.js/chart.js/eslint-10/vendoring shim.
**Checkpoint:** vitest unit (chat's 20+ `.test.ts` + ported moc logic: validation/cron/ModelPicker/file-upload; new `McpServerPicker` rendering identically in `mode="conversation"` and `mode="task"`; `CredentialAccountAdmin` write-only-secret assertion; `ConcurrencyCapSetting`); chat `auth.test.ts`/`middleware.test.ts` extended to assert the widened matcher gates `/orchestrator/*` and BOTH login paths resolve; Playwright mocked (`CHAT_MOCK_MODE=1`): `/chat` login+stream and `/orchestrator` task-create (incl. MCP enable + account select)+list+log-view, plus one elcano_auth session navigating Chat↔Orchestrator without re-login.

### P8 — End-to-end single-host fleet (chat + sandbox + scheduling, one box)
**Moves:** full deploy via unified `bootstrap.sh` (test `--postgres=local` AND `--postgres=external`, idempotent re-run, non-interactive). One systemd `fleet.target`.
**Checkpoint:** **Live E2E** (Playwright `test:e2e:live`, real OpenRouter): an interactive chat turn runs `bash`+`run_python` inside a rootless-Podman sandbox container; the orchestrator schedules a recurring task with `mcp_selection=[{xandr,client_a},{magnite}]` whose cron triggers, gets leased by the in-process pool under the global cap, runs the unified runtime through the SAME sandbox over its persistent workspace, and reports `success` + logs back to the React log viewer. Assert: xandr subprocess saw `XANDR_*_CLIENT_A` creds while magnite saw the default seat; disabled servers contributed no tools; SSE streaming; sandbox hardening flags; both DB pools; single elcano_auth session across both views; the cap holds under a burst.

---

## 9. Remaining open decisions

The container per-turn-vs-per-session choice is **RESOLVED** by the container-model analysis: the **hybrid wins** — per-turn (interactive) / per-exec-burst (scheduled) ephemeral container over a persistent same-path workspace; pure per-session long-lived containers are rejected (they reopen the cross-conversation leak and are the exact "container wedges, lose the whole session" failure to avoid).

Crisp remaining questions:

1. **`FLEET_MAX_CONCURRENT_AGENTS` default and warm-pool derivation** — confirm default 4, and whether warm Size auto-derives as `min(2, cap)`.
2. **Cap fairness** — do interactive turns and scheduled tasks each count as 1 unit (simple, recommended), or do interactive turns get reserved headroom so a scheduled burst can't lock out live chat?
3. **Scheduled sandbox granularity** — one container per inner step (max isolation, more cold-starts) vs one container held for the whole scheduled run's exec-burst then torn down (recommended). Confirm exec-burst.
4. **Shared variant subprocess** — when a scheduled task and an interactive conversation both select the same `(server, non-default account)`, share one spawned subprocess (current keying by `server_account`, saves memory, couples MCP state) or isolate them?
5. **Account in the chat UI** — surface the per-MCP account selector in the chat toolbar too, or keep account selection scheduled-only for now (chat exposes only server on/off today)?
6. **Default-account semantics** — when a server has accounts configured but a task omits `Account`: fall back to the shared base var (gamma's behavior), or REFUSE and force explicit selection (cutlass's stricter stance, safer for unattended runs)?
7. **Workspace retention/GC** — persistent scheduled-task subdirs accumulate with no container holding them. Keep last-N runs, age out after T, or keep until task deleted?
8. **Scheduled network default** — do scheduled tasks default to `NoNetwork` (lockdown) or inherit the slirp4netns default, exposed as a per-task toggle alongside MCP-enable + account?
9. **Lease window configurability** — keep the hardcoded 5-min lease + internal renewal, or make lease window + renewal interval configurable for long-running scheduled agents?
10. **Synthetic worker identity** — keep a real `nodes` row (`__fleet_local__`) so lease/ownership FKs and `TestRecoveredTaskRejectsOldNode` are untouched (lower-risk), or drop the nodes table and use a sentinel `lease_owner` string (cleaner long-term)?
11. **Account naming normalization** — unify cutlass's uppercase-suffix/lowercase-client convention with chat's email-local-part derivation so `Client_A` and `client-a` don't fork seats.
