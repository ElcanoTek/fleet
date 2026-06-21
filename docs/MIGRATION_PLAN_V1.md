Confirmed: chat uses `jackc/pgx/v5`, moc uses `lib/pq` — a driver divergence to note. All facts gathered. Here is the plan.

---

# Fleet Migration Plan — Consolidating chat, moc, cutlass, gig + lifeline into one Go monorepo

**Author:** Lead Architect · **Date:** 2026-06-21 · **Status:** For review, then execution

---

## 1. Executive summary

- **Canonical module path: `github.com/ElcanoTek/fleet`** (CamelCase). This is forced — the fleet git remote is already `git@github.com:ElcanoTek/fleet.git`, and 3 of 4 Go repos (moc/cutlass/gig) already use `github.com/ElcanoTek/*`. Only chat's lowercase `github.com/elcanotek/chat/server` changes. Go is case-sensitive on import paths, so this must be decided before any code moves.
- **Single Go module, multi-package layout** (`/cmd` + `/internal`). One `go.mod` at `/root/fleet`, Go 1.26.4 (chat/cutlass already there; moc 1.25→1.26, gig 1.22→1.26). This is what lets us extract a *shared* `internal/agentcore` consumed by both the interactive and scheduled drivers — the central goal.
- **Mega Box = one `fleet` binary, two long-running servers + one scheduler in-process**: the interactive chat HTTP/SSE server (chat-server), the scheduler/orchestrator HTTP server (moc), and the 30s scheduler goroutine — all in a single process, both sandbox workloads via the same rootless-Podman `sandbox.Pool` contract.
- **MCP dedup is a chat-vs-cutlass exercise only** (moc has zero MCP code). Rule of thumb: **cutlass wins on the DSP Python servers** (newer, larger; chat's last MCP commit literally says "port SSP reporting fixes from cutlass"), **merge the Go client and pubmatic/sendgrid** (genuine bidirectional divergence), **keep chat's gamma.py + mailbux.py** (chat-only).
- **Agent core unifies behind three seams** — Observer (SSE sink vs JSON log), Enforcement hooks (interactive approvals/memories/ceilings vs batch audit/finish gating), and Executor (Podman container vs direct exec). We do **not** force chat to go one-shot or cutlass into a container; we preserve both designs exactly behind the Executor seam.
- **One frontend (chat's Next.js 16) with two views**: View A = existing `ChatExperience` at `/chat`, View B = a React **re-port** of moc's dashboard at `/orchestrator` (moc has no React — it's Go-embedded HTML + vanilla ES6, so it must be reimplemented, not imported). Both gated by the already-shared Elcano Ed25519 `elcano_auth` cookie.
- **Bootstrap gains a `--postgres=local|external` branch**; one Postgres cluster with **two databases** (`chat` + `sched`), never a merged schema (the two `users` tables are structurally incompatible). gig stays its own external repo; **lifeline stays external** (per-developer tool, no fleet coupling).
- **Phased, test-gated migration (P0→P7)** starts with the lowest-risk shared island (`internal/mcp`) → agentcore → drivers → backends wired into one process → frontend → bootstrap → E2E across chat+sandbox+scheduling. Every phase has a named existing test suite that must stay green before proceeding.

---

## 2. Canonical module path & Go module strategy

### 2.1 Resolving the casing inconsistency

| Repo | Current module path | Casing |
|---|---|---|
| chat | `github.com/elcanotek/chat/server` | lowercase |
| moc | `github.com/ElcanoTek/moc` | CamelCase |
| cutlass | `github.com/ElcanoTek/cutlass` | CamelCase |
| gig | `github.com/ElcanoTek/gig` | CamelCase |

**Decision: the canonical module path is `github.com/ElcanoTek/fleet`.**

This is not a coin-flip — it is forced by ground truth I verified:
- The fleet repo's git remote is already `git@github.com:ElcanoTek/fleet.git` (CamelCase org).
- 3 of 4 Go repos already use `github.com/ElcanoTek/*`. Only chat is the outlier.

So the rewrite cost is asymmetric and minimized by choosing CamelCase: every import inside the chat code (`github.com/elcanotek/chat/server/internal/...`) is rewritten exactly once during its move; moc/cutlass import prefixes shorten (`.../moc/internal/...` → `.../fleet/internal/...`) but the casing of the org segment is unchanged. GitHub repo URLs are case-insensitive for fetching, but **Go import paths are case-sensitive**, so a clean cutover (not a redirect) is mandatory.

### 2.2 Single module vs multi-module

**Recommendation: single Go module** rooted at `/root/fleet/go.mod`, `module github.com/ElcanoTek/fleet`, `go 1.26.4`.

Justification:
- **The whole point of consolidation is the shared `internal/agentcore` package** consumed by *both* the interactive and scheduled agent drivers. A single module makes `internal/agentcore` a normal internal import for both `cmd/fleet`'s subsystems. Multi-module would force either a `replace` web or publishing the core as a versioned dependency — pure friction for a monorepo that always builds together.
- `internal/` enforces the boundary we want: nothing outside fleet can import the agent core, store, or sandbox internals.
- All four Go modules already pin **`charm.land/fantasy v0.31.0`** identically, so there is no agent-framework version conflict to reconcile at the module level.
- Driver divergence to flag: chat uses `jackc/pgx/v5 v5.10.0`, moc uses `lib/pq v1.12.3`. A single module can carry both during transition, but we should converge the scheduler onto `pgx` (see §8) to drop `lib/pq`.

**gig is the one exception** and stays a separate repo (`github.com/ElcanoTek/gig`): it is a standalone runner deployed on *different* machines (dedicated runners), with an independent release cycle, only `github.com/google/uuid` as a dependency, and it talks to the orchestrator purely over HTTPS. Pulling it into the fleet module would couple a remote runner's release to the Mega Box's. Keep it out; document its API contract against the consolidated scheduler.

---

## 3. Proposed fleet directory layout

```
/root/fleet/
├── go.mod                          # module github.com/ElcanoTek/fleet, go 1.26.4
├── go.sum
├── README.md
├── Makefile                        # build/test/lint (merge of chat + cutlass + moc targets)
│
├── cmd/
│   ├── fleet/                      # THE Mega Box binary: chat HTTP server + scheduler HTTP
│   │   └── main.go                 #   server + 30s scheduler goroutine, all in one process
│   ├── fleet-admin/                # unified admin CLI (bootstrap, chat/sched user add|update…)
│   │   └── main.go                 #   absorbs chat-admin + moc's -create-user/-set-role flags
│   ├── cutlass/                    # one-shot agent CLI (gig invokes this in a container)
│   │   └── main.go                 #   --task/--env; the scheduled-agent entrypoint
│   └── sandbox-probe/              # deploy-time sandbox smoke test (Pool.Take/TakeContainer)
│       └── main.go
│
├── internal/
│   ├── agentcore/                  # ★ SHARED agent primitives (provider, MCP wrap, cache,
│   │                               #   stream-classify, shared orchestration helpers)
│   ├── agent/
│   │   ├── interactive/            # chat front-end driver (was chat/internal/agent):
│   │   │                           #   session.go, finalize.go, overflow.go, summarize.go…
│   │   └── scheduled/              # cutlass front-end driver (was cutlass/internal/agent):
│   │                               #   Execute() loop, session_log.go, verifier.go, mcp_loader.go
│   ├── mcp/                        # ONE merged Go MCP client (stdio+HTTP)
│   ├── sandbox/                    # ONE sandbox backend (container.go, host.go, pool.go, bridge)
│   ├── tools/                      # native tools; bash/run_python parameterized by Executor seam
│   ├── store/                      # interactive (chat) Postgres layer + migrations
│   │   └── migrations/             #   001_initial.sql … (hand-rolled, advisory-lock runner)
│   ├── sched/                      # scheduler/orchestrator (was moc):
│   │   ├── handlers/  scheduler/  storage/  models/  apikeys/
│   │   └── db/
│   │       └── migrations/         #   001..013 *.up.sql/*.down.sql (golang-migrate)
│   ├── httpapi/                    # chat HTTP/SSE layer (auth, turn buffer)
│   ├── config/                     # unified config (URL-based DSN builder, FLEET_ prefix)
│   └── bootstrap/                  # Go-side bootstrap helpers (DSN validate, role/db create)
│
├── web/                            # the single Next.js 16 app (from /root/chat)
│   ├── package.json                # chat's stack: next 16.2.9, react 19.2.7, tailwind 4, vitest
│   ├── middleware.ts               # unified: elcano_auth gate for /chat/* and /orchestrator/*
│   ├── next.config.ts
│   ├── public/                     # merged assets (chat icons + Elcano marks; dedup)
│   ├── e2e/                        # merged Playwright (chat e2e + moc frontend.spec.ts ported)
│   └── src/
│       ├── app/
│       │   ├── layout.tsx          # fleet root layout (Geist, theme.js, merged globals.css)
│       │   ├── page.tsx            # landing / view switcher (or redirect to /chat)
│       │   ├── chat/               # View A: page.tsx + page-client.tsx + ui/ChatExperience
│       │   ├── orchestrator/       # View B: React re-port of moc dashboard
│       │   ├── api/
│       │   │   ├── chat/*          # proxy → chat server (X-Chat-Server-Token + X-User-Email)
│       │   │   └── orchestrator/*  # proxy → scheduler API (mocServer.ts, scaffolded from openapi)
│       │   └── lib/                # shared: auth.ts (Ed25519), csrf.ts, sse, chatServer.ts, mocServer.ts
│       └── shared/                 # shared components/hooks/types
│
├── images/
│   └── sandbox/
│       └── Containerfile           # ★ ONE authoritative sandbox image (was /root/sandbox +
│                                   #   chat/deploy/sandbox.Containerfile replica)
│
├── deploy/
│   ├── fleet.target               # systemd: fleet-server.service + fleet-web.service
│   ├── fleet-cli                  # single operator dispatcher (absorbs chat-cli + moc-cli)
│   ├── cutlass/Containerfile       # the agent's OWN runtime image (multi-stage Go + Fedora)
│   └── Caddyfile                   # optional TLS (non-localhost hostname)
│
├── scripts/
│   ├── bootstrap.sh                # unified bootstrap with --postgres=local|external branch
│   ├── build-sandbox-image.sh      # builds images/sandbox/Containerfile → localhost/fleet-sandbox
│   └── validate-migrations.sh      # moc's check, retained for the sched migrations
│
├── personas/                       # victoria.yaml (+ any chat personas) — shared, stable abs path
├── protocols/                      # cutlass workflow YAML/MD (scheduled-agent specific)
├── system_prompts/                 # default.md (one-shot ACI) + chat system prompt
└── mcp/                            # ★ ONE deduped set of Python MCP servers (see §5)
    ├── email_lint.py  sendgrid_server.py  ses_s3_email.py
    ├── xandr_mcp.py  magnite_mcp.py  medianet_mcp.py  pubmatic_mcp.py
    ├── indexexchange_mcp.py  openx_mcp.py  triplelift_mcp.py  deal_sheet_server.py
    ├── gamma.py  mailbux.py          # chat-only, kept
    └── tests/                        # consolidated pytest (test_*_mcp.py, test_mcp_integration.py)
```

Notes on placement decisions:
- **Frontend** lives at `/web` (one Next app, two route segments). **Sandbox Containerfile** is canonicalized at `/images/sandbox/Containerfile` (kills the chat/deploy replica). **Migrations** stay co-located per data layer (`internal/store/migrations` and `internal/sched/db/migrations`) because the two engines are incompatible (§8). **personas/protocols/system_prompts** sit at stable top-level absolute paths so the sandbox can same-path bind-mount them read-only. **lifeline does not appear** — it stays external (§10).

---

## 4. Source → target mapping

### chat (`github.com/elcanotek/chat/server`)

| Source | Fleet target | Note |
|---|---|---|
| `internal/agent/{fantasy,cache,resilience,orchestration}.go` (shared parts) | `internal/agentcore/` | **Merge** — extract provider/cache/stream-classify/`openrouterCost`/`checkRepeatedCall` into shared core (§6) |
| `internal/agent/{session,finalize,overflow,summarize,mcp_output_dir,models,doc}.go` | `internal/agent/interactive/` | **Keep** — multi-turn/SSE/Postgres-replay; chat-specific |
| `internal/agent/mcp_fastio_response.go` (+ test) | `internal/agentcore/` | **Keep either** — byte-identical with cutlass |
| `internal/agent/mcp_fastio_guard.go` | `internal/agentcore/` | **Merge** — parameterize remediation-hint string |
| `internal/mcp/` | `internal/mcp/` | **Merge** (base on chat) — see §5 ledger |
| `internal/sandbox/` | `internal/sandbox/` | **Keep** — authoritative sandbox backend (cutlass has none) |
| `internal/store/` + `migrations/*.sql` | `internal/store/` | **Keep** — interactive Postgres layer + 8 hand-rolled migrations |
| `internal/httpapi/` | `internal/httpapi/` | **Keep** — chat HTTP/SSE/auth |
| `internal/tools/` (bash, python_repl, fs, web_*, xlsx…) | `internal/tools/` | **Merge** with cutlass tools; bash/python parameterized by Executor seam |
| `internal/config/` | `internal/config/` | **Merge** — keep chat's URL-escaping DSN builder as the standard |
| `cmd/chat-server/main.go` | `cmd/fleet/main.go` | **Merge** — becomes one subsystem of the Mega Box |
| `cmd/chat-admin/main.go` | `cmd/fleet-admin/main.go` | **Merge** — folds into unified CLI verb tree |
| `cmd/sandbox-probe/main.go` | `cmd/sandbox-probe/main.go` | **Keep** — extend to cover scheduled agents |
| `server/mcp/{email_lint,sendgrid_server,ses_s3_email,*dsp*}.py` | `mcp/` | **Mostly drop in favor of cutlass** (§5); keep `gamma.py`, `mailbux.py` |
| `deploy/sandbox.Containerfile`, `scripts/build-sandbox-image.sh` | `images/sandbox/`, `scripts/` | **Drop replica**; one authoritative Containerfile |
| `src/` (Next.js app) | `web/src/app/chat/` + `web/src/app/lib/` | **Keep/relocate** — becomes View A + shared shell (§7) |

### moc (`github.com/ElcanoTek/moc`)

| Source | Fleet target | Note |
|---|---|---|
| `internal/scheduler/scheduler.go` | `internal/sched/scheduler/` | **Keep** — 30s ticker runs as goroutine in Mega Box |
| `internal/storage/`, `internal/handlers/`, `internal/models/`, `internal/apikeys/` | `internal/sched/{storage,handlers,models,apikeys}/` | **Keep** — orchestrator logic, self-contained |
| `internal/db/` + `migrations/*.{up,down}.sql` | `internal/sched/db/` | **Keep** — golang-migrate, 13 pairs; converge driver `lib/pq`→`pgx` |
| `cmd/moc/main.go` | `cmd/fleet/main.go` (sched subsystem) + `cmd/fleet-admin` | **Merge** — server into Mega Box; `-create-user`/`-set-role`/etc. into CLI |
| `cmd/moc/templates/dashboard.html` + `assets/js/*.js` | `web/src/app/orchestrator/` | **Merge (re-port)** — reimplement vanilla ES6 as React (§7) |
| `deploy/{moc.service,moc-cli,Caddyfile}` | `deploy/` | **Merge** into `fleet.target`, `fleet-cli`, shared Caddyfile |
| `tests/e2e/frontend.spec.ts` | `web/e2e/` | **Merge** into unified Playwright suite |
| MCP | — | **N/A** — moc has zero MCP code |

### cutlass (`github.com/ElcanoTek/cutlass`)

| Source | Fleet target | Note |
|---|---|---|
| `internal/agent/{fantasy,cache,resilience,orchestration}.go` (shared parts) | `internal/agentcore/` | **Merge** — fold `HasServer`, double-execution guard, 4th cache breakpoint (§6) |
| `internal/agent/{agent(Execute),session_log,livelog,mcp_loader,verifier,toolresult,openrouter_models}.go` | `internal/agent/scheduled/` | **Keep** — one-shot/persona/captain's-log driver |
| `internal/agent/mcp_fastio_guard.go` | `internal/agentcore/` | **Drop** — merged into shared (cutlass's native-tool hint preserved as one option) |
| `internal/mcp/client.go` | `internal/mcp/` | **Merge** — fold `HasServer` + `isRequestNotDeliveredError` into chat-base (§5) |
| `internal/tools/{bash,python_repl,task_tracker,web_fetch}.go` | `internal/tools/` | **Merge** — cutlass's direct-exec bash becomes the `scheduled` Executor impl |
| `internal/config/` | `internal/config/` | **Merge** into unified config (carry cutlass's allowlist breadth) |
| `internal/captainslog/` | `internal/captainslog/` (used only by scheduled driver) | **Keep** — cutlass-specific |
| `mcp/{xandr,magnite,medianet,indexexchange,pubmatic,openx,triplelift}_mcp.py`, `deal_sheet_server.py`, `ses_s3_email.py` | `mcp/` | **Keep cutlass's** (newer/superset); **pubmatic = merge** (§5) |
| `mcp/{email_lint,sendgrid_server}.py` | `mcp/` | **Merge** — fold chat's helpers into cutlass copy (§5) |
| `protocols/`, `system_prompts/default.md`, `personas/victoria.yaml`, `data/*.csv` | `protocols/`, `system_prompts/`, `personas/`, `data/` | **Keep** — scheduled-agent workflow layer |
| `cmd/cutlass/main.go`, `Containerfile` | `cmd/cutlass/`, `deploy/cutlass/Containerfile` | **Keep** — one-shot entrypoint + agent runtime image |

### gig (`github.com/ElcanoTek/gig`)

| Source | Fleet target | Note |
|---|---|---|
| entire repo (`main.go`) | — | **Drop from fleet module** — stays its own repo `github.com/ElcanoTek/gig` (independent release, runs on remote runners). Document its `/register`, `/tasks/pending`, `/status`, `/logs`, `/nodes/heartbeat` contract against the consolidated scheduler. |

### sandbox + lifeline

| Source | Fleet target | Note |
|---|---|---|
| `/root/sandbox/Containerfile` | `images/sandbox/Containerfile` | **Keep (canonical)** — decide base image (§10) |
| `/root/lifeline/` | — | **Drop from fleet** — keep external per-developer tool (§10) |

---

## 5. MCP deduplication ledger

moc contributes nothing (verified: no `*.py`, no `fastmcp`, no `internal/mcp`). This is entirely chat-vs-cutlass.

| Artifact | Decision | Winner / action | Rationale (grounded) |
|---|---|---|---|
| **Go `internal/mcp/client.go`** | **MERGE** | Base = chat; fold in cutlass's `HasServer` + `isRequestNotDeliveredError`; adopt cutlass's hoisted JSON-RPC constants + `matchesInt`; keep chat's `doc.go` | Neither is a superset (~424-line normalized diff). chat needs `CallToolOn`/`CallToolPrefixed` (sendgrid+mailbux both export `send_email` → server-scoped dispatch required) and forwards subprocess stderr to journalctl + safer `isTransportDeadError`. cutlass needs `HasServer` (load-on-demand idempotency) + non-idempotent retry guard. **Framing: use chat's per-call single-reader** (≤1 orphaned reader), layer cutlass's write-vs-read delivery distinction on top. |
| `agent/mcp_fastio_response.go` (+test) | **KEEP EITHER** | Pick one, fix import path, delete other | `diff` exit 0 — byte-identical (251 LOC). |
| `agent/mcp_fastio_guard.go` | **MERGE** | Identical guard logic; parameterize remediation-hint via `RemediationHints` struct; keep cutlass's extracted goconst constants | Same 10KB inline cap, same field iteration, same server-enabled probe. Only the hint text differs (chat → blob flow; cutlass → native `fastio_upload_file`). Expose **both** in one hint. |
| `email_lint.py` | **KEEP EITHER** | One canonical copy | byte-identical, 1151 LOC each. |
| `sendgrid_server.py` | **MERGE** | Combine helpers (chat's `_check_template_leakage`+`_read_file_content` + cutlass's `_validate_path_security`) | Identical 5-tool surface, same last-commit date; only helper-level differences. Reconcile, don't pick a loser. |
| `ses_s3_email.py` | **KEEP CUTLASS** | strict superset | cutlass 2815 vs chat 2575 LOC; adds `find_latest_report` (chat has 0 occurrences); no chat-only tool. |
| `xandr_mcp.py` | **KEEP CUTLASS** | 2954 LOC/11 tools vs 920/9 | cutlass newer (2026-06-12) + create/update deal ops; chat's last commit literally "port SSP reporting fixes from cutlass" (chat is downstream). |
| `magnite_mcp.py` | **KEEP CUTLASS** | 2142 LOC/26 tools vs 985/10 | 2.6× surface (ClearLine Curation Demand Mgmt); chat is a reporting-only subset. |
| `medianet_mcp.py` | **KEEP CUTLASS** | 3154 LOC/16 vs 1292/15 | cutlass newer + richer. **Per-tool diff first** (counts close — confirm no chat-only tool dropped). |
| `indexexchange_mcp.py` | **KEEP CUTLASS** | 5699 LOC/28 vs 4694/26 | cutlass newer + larger. Reconcile the 2-tool delta. |
| `pubmatic_mcp.py` | **MERGE (no blind overwrite)** | Base = cutlass (prepared-deal: `pm_create_prepared_deal`/`pm_prepare_deal_from_prompt_inputs`/`pm_update_curated_deal[_status]`); **port chat's discovery tools** (`pm_discover_dsps`/`pm_discover_dsp_buyer_map`/`pm_create_targeting`/`pm_get_targeting`) | Genuinely diverged — each has ~6 tools the other lacks. Prepared-deal aligns with cutlass audit gating; discovery tools may be invoked by chat protocols. Keep both. |
| `openx_mcp.py`, `triplelift_mcp.py`, `deal_sheet_server.py` | **KEEP CUTLASS** | cutlass-only | Absent from chat; carry as-is. |
| `gamma.py`, `mailbux.py` | **KEEP CHAT** | chat-only | gamma = Optional per-conversation opt-in MCP; mailbux = chat's Victoria Terminal JMAP/SMTP. Neither belongs to the one-shot agent. |
| **moc MCP** | **NONE** | — | moc has no MCP code. |

---

## 6. Agent-core unification

### 6.1 Reality check

The two agents do **not** implement the same design — so "preserve the design exactly" means *extract the shared primitives and keep both front-ends behind a seam*, **not** make them converge. Verified divergences: chat's bash takes `NewBashTool(sb *sandbox.Sandbox)` and imports `internal/sandbox`; cutlass's `NewBashTool()` runs `exec.CommandContext(cmdCtx, "bash", "-c", command)` directly with no sandbox import. chat is long-lived multi-turn (Postgres replay, SSE, per-conversation MCP opt-in); cutlass is one-shot (pinned primary+fallback model, maxIterations, load-on-demand MCP, captain's-log PR).

### 6.2 Shared package: `internal/agentcore`

Move the genuinely front-end-agnostic primitives here, with every shared function taking its state **explicitly** (not hanging off `*Manager`/`*Agent`):

- **Provider/model**: `newOpenRouterProvider`, `upstreamPinFor`, `isAliasModel` (same functions today; only receiver differs).
- **MCP tool wrapping**: `mcpTool`, `sanitizeSchemaProperties`/`sanitizeSchemaValue`, the `buildFantasyTools` skeleton.
- **Prompt cache**: all of `cache.go` (`shouldCacheModel`, `cacheMarker`, breakpoint placement). cutlass's 4th compaction-summary breakpoint is an additive option.
- **Stream classification**: `resilienceConfig`, `loadResilienceConfig`, `classifyStreamError`, `streamErrorClass`.
- **Shared orchestration helpers**: `openrouterCost` (byte-identical), `hashString`, `checkRepeatedCall` (differs by **one word** — "reply to the user" vs "finish the task"; parameterize the noun), and the `updateUsage`/`recordToolResult` skeleton.
- **fast.io guard + trimmer** (§5).
- **Env-var prefix parameterized**: constructor takes the prefix (`FLEET_` canonical, with `CHAT_`/`CUTLASS_` read as backward-compat aliases) instead of a compile-time literal.

### 6.3 The seam: three interfaces between core and drivers

```
            ┌─────────────────────────────────────────────┐
            │            internal/agentcore               │
            │  provider · model · MCP-wrap · cache ·       │
            │  stream-classify · cost · repeated-call      │
            └───────┬───────────────┬──────────────┬───────┘
        Seam 1      │      Seam 2    │     Seam 3   │
       Observer     │   Enforcement  │   Executor   │
   ┌────────────────▼──┐  ┌──────────▼───┐  ┌───────▼────────────┐
   │ EventSink (SSE)   │  │ approvals/   │  │ sandbox.Sandbox    │  ← interactive
   │ JSON LogSession   │  │ memories/    │  │ direct exec+pathsec│  ← scheduled
   │                   │  │ ceilings     │  │                    │
   │                   │  │ audit/finish │  │                    │
   └───────────────────┘  └──────────────┘  └────────────────────┘
```

- **Seam 1 — Observer** (`interface { Emit(event string, payload any) }`, generalize chat's `EventSink`): interactive impl = the SSE `turn_buffer`; scheduled impl = a `LogSession` adapter (`AddMessageWithMetadata`).
- **Seam 2 — Enforcement hooks** (`BeforeToolCall(name, rawInput) (block bool, msg string)`; `RecordToolResult(...)`): interactive plugs in email rate-limit/dedup, `ApprovalStager` (send_email + risky-bash human gate), `MemoryProposer`, suggest_advanced nudge, cost/token ceilings; scheduled plugs in `registerCommittedActions`/`checkCriticalTool`/`checkFinishEnforcement` + `confirm_audit`.
- **Seam 3 — Executor** (`CodeExecutor { RunBash(...); RunPython(...) }`): interactive supplies a `sandbox.Sandbox`-backed impl (per-turn Podman container, warm pool); scheduled supplies a pathsec-gated direct-exec impl. **The core never knows which backend runs.**

### 6.4 What we explicitly do NOT converge

Per the constraint to preserve design exactly: chat keeps container-sandboxed bash/python + overflow-file context spill; cutlass keeps direct-exec bash + force-compaction/fallback-model-swap. Both plug into the core via Seam 3 + a `PrepareStep`/compaction hook. "Should cutlass also containerize?" is a **separate future decision**, out of scope here.

**Open design choice (for the user, §10):** does the fantasy run-loop body live *in* `agentcore` (with hooks), or stay per-driver (`session.go::RunTurn` vs `agent.go::Execute`) calling shared helpers? Recommendation: keep the loop per-driver initially (stop conditions, prepare-steps, and enforcement ordering genuinely differ), share only the primitives — lower risk, revisit after P3.

---

## 7. Frontend unification

**One Next.js 16 app** (chat's — moc has no React, only Go-embedded `dashboard.html` + 12 vanilla ES6 modules that **cannot be imported, only re-ported**). chat's stack is the shell: `next 16.2.9`, `react/react-dom 19.2.7`, App Router, Tailwind 4, vitest, Playwright.

**Two views:**
- **View A — Chat** at `/chat`: lift the existing `ui/ChatExperience` (≈352KB component) + `lib/*` verbatim; relocate `page.tsx` + `page-client.tsx` under the segment. `page-client.tsx` already `dynamic(... { ssr:false })`-imports `ChatExperience`, so it drops into any route unchanged. All 30+ `/api/*` proxy routes follow.
- **View B — Orchestrator** at `/orchestrator`: **re-port** moc's `dashboard.html` + `dashboard.js`/`tasks.js`/`modals.js`/`file-upload.js`/`model-picker.js`/`validation.js` as React components/hooks. Do **NOT iframe** — both moc (`script-src 'self'`, `X-Frame-Options: DENY`) and chat (`X-Frame-Options: DENY`) block cross-embedding. The task-create form (prompt, model/fallback_model, max_iterations, target_node_name, scheduled_for, recurrence cron, captain's-log flag) is the orchestration UI; `dashboard.html` is the authoritative field/ID spec.

**Shared auth — already unified (the biggest win):** both apps verify the identical Elcano Ed25519 `elcano_auth` cookie — chat in `src/app/lib/auth.ts` (`verifyElcanoToken`), moc in `internal/handlers/elcano.go` (documented byte-for-byte mirror), both keyed on `AUTH_SIGNING_PUBKEY`. One Next `middleware.ts` gates both `/chat/*` and `/orchestrator/*`. Keep chat's HMAC `elcano_session` password path as secondary login.

**Shared layout/theme:** keep chat's `layout.tsx` (Geist fonts, `theme.js` beforeInteractive); converge on chat's `globals.css` CSS variables, port moc's `design-tokens.css` values in (both already use `data-theme` on `<html>` and the same dark base `#1a0b1e` / primary `#7272ab` — clean merge). Add a top-level nav switcher.

**Backend talk — two Go surfaces, both proxied through Next:** there is no single Go backend today. Keep chat's `chatServer.ts` proxy (`X-Chat-Server-Token` + `X-User-Email` → :8080) and **add `mocServer.ts` + `/api/orchestrator/*`** proxy (scaffolded from `moc/openapi.yaml`, → :8000). View B calls `/api/orchestrator/{stats,tasks,nodes,logs}` instead of moc's same-origin paths. Namespace to avoid collisions (orchestrator under `/api/orchestrator/*`). **Note:** even when both servers run inside the *one* `fleet` process (§3), they still expose two HTTP listeners; the Next proxy keeps one browser origin over both.

**Tooling to reconcile (standardize on chat):** ESLint 9 (chat) over 10 (moc); vitest over jest; react-markdown stack over moc's marked+DOMPurify+highlight.js (use chat's renderer for the ported log viewer, mapping moc's `logs` JSONB session shape into it). Retire moc's `package.json` vendoring shim entirely.

---

## 8. Bootstrap & DB

### 8.1 Unified CLI

Ship **one `fleet-admin` binary** with a service-scoped verb tree, fronted by one `deploy/fleet-cli` dispatcher (absorbing `chat-cli` + `moc-cli`):

```
fleet bootstrap [--postgres=local|external] [--services="chat sched"]
fleet chat  user add <email> [--password -] | update | del | list
fleet sched user add <name> [--role …] [--scopes …] | update | del | list | set-role | rename
```

- **`user add`** maps to chat's `CreateUser` (email+bcrypt) and moc's `-create-user` (username+bcrypt+role+scopes).
- **`user update`** is the umbrella verb: chat → `UpdatePassword` (`store/users.go`); sched → `set-role` + `rename` + passwd.
- **Move ALL logic into Go** — delete the `moc-cli` bash/psql hacks (e.g. `moc user passwd` creating a throwaway user then `UPDATE`-ing the hash via psql; `moc user list`/`rename` done over psql because the binary lacks those flags). One auditable Go path.
- **Preserve security invariants already coded:** passwords never on argv (stdin via `--password -`, auto-generate otherwise), `bcrypt.DefaultCost`, email normalization (lowercase/trim), and the 0-users "unprovisioned" guard (`store.CountUsers`).

### 8.2 Local-vs-managed Postgres bootstrap branch

Single switch consumed once at the top of `fleet bootstrap`: env `INFRA_POSTGRES_MODE=local|external` or `--postgres=local|external` (**default `local`** for backward-compat). Then branch:

**Branch A — Local** (verbatim refactor of today's chat/moc bootstrap into a shared function):
1. `dnf install postgresql-server postgresql-contrib`.
2. `initdb` if `/var/lib/pgsql/data/PG_VERSION` absent (`postgresql-setup --initdb`; direct `initdb --auth-local=peer --auth-host=scram-sha-256` fallback for the systemd-less dry-run container).
3. Rewrite `pg_hba.conf` loopback host rules `ident → scram-sha-256` (same sed both bootstraps already use).
4. `systemctl enable --now postgresql`.
5. Per service, idempotent role+db via the `\gexec` pattern (`CREATE ROLE … LOGIN PASSWORD … WHERE NOT EXISTS in pg_roles`; `ALTER ROLE` to (re)set the reused-or-generated password; `CREATE DATABASE OWNER … WHERE NOT EXISTS`; `GRANT ALL`). **Reuse existing password** if a prior `DATABASE_URL` is present (parse `postgres://<svc>:…@`) so re-runs don't rotate. `sslmode=disable` (loopback).

**Branch B — External managed** (purely subtractive + a probe):
1. **Skip** dnf-postgres, initdb, pg_hba, systemctl entirely.
2. Read DSN (`INFRA_POSTGRES_URL` or `INFRA_DB_{HOST,PORT,USER,PASSWORD}` mirroring the existing `DB_*` contract).
3. Validate connectivity: `psql "$DSN" -c 'SELECT 1'`, **fail fast** with an actionable message.
4. **Default: assume per-service roles+databases pre-provisioned** (managed-PG admins rarely hand out superuser). **Opt-in**: if a superuser DSN is supplied, run the same `\gexec` role+db creation against the external cluster.
5. Write each service's `DATABASE_URL` to its env file with **`sslmode=require`** (managed providers typically require TLS — override the local `disable` default).

Both branches converge on the same output: a correct `DATABASE_URL` in each service's env, after which the server code is unaware of which branch ran. **Bootstrap never runs migrations** — services self-migrate on first start (chat's advisory-lock runner; moc's golang-migrate). This is what makes Branch B trivial.

### 8.3 Where config + migrations live

- **Connection config**: keep the dual contract (`DATABASE_URL` preferred, `DB_*` fallback). **Standardize on chat's URL-form DSN builder** (`url.UserPassword` correctly escapes `@`/`:`/`#` in passwords — moc's naive `host=… port=…` concatenation does not). Centralize in `internal/config`, vending per-service DSNs.
- **Migrations co-located per data layer, NOT centralized** — the two engines are incompatible: chat's hand-rolled embedded runner (`internal/store/migrations.go`, `pg_advisory_lock`, per-file transaction, `schema_migrations` table, forward-only, refuses downgrade) vs moc's golang-migrate (paired up/down). Keep both as-is for v1; keep `validate-migrations.sh` for the sched set. **Driver convergence**: migrate sched off `lib/pq` onto `pgx` to drop a dependency (v2 nicety, not blocking).

### 8.4 One DB vs multi-schema

**Decision: one Postgres cluster, TWO logical databases (`chat` + `sched`), two owner roles, two `DATABASE_URL`s — NOT a merged schema.**

The two services have disjoint table sets with their own `schema_migrations` tracking, and both define a **`users` table with structurally incompatible shapes** (chat: `email` PK + `password_hash` + timestamps, no role; sched: `id` UUID + `username` + `password_hash` + `role` + `scopes` JSONB + session token). Merging schemas collides on `users` for zero benefit. The single `fleet` process opens two independent `*sql.DB` pools (chat via `store.Open`, scheduler via `db.Init`), each 25/5/5m, sharing a cluster not a schema. Cross-service reads (if ever needed) go through the app layer.

**Fallback only if a managed plan bills per-database:** two Postgres *schemas* (search_path-separated) in one database — but neither migration runner is schema-qualified today, so this is strictly more work and is the non-default.

---

## 9. Phased migration sequence

Each phase ends with a **test checkpoint** that must pass before the next begins. Start with the shared core + MCP dedup, end with E2E across chat+sandbox+scheduling.

### P0 — Module skeleton & casing cutover
**Moves:** create `/root/fleet/go.mod` (`github.com/ElcanoTek/fleet`, go 1.26.4), the `/cmd` + `/internal` skeleton, root Makefile. Decide casing (already forced CamelCase, §2).
**Why first:** every subsequent import rewrite depends on the canonical path being fixed.
**Test checkpoint:** `go build ./...` on the empty skeleton; CI lint config (golangci-lint v2 from cutlass) loads. No app tests yet.

### P1 — `internal/mcp` (lowest-risk shared island)
**Moves:** merge chat+cutlass Go MCP clients per §5 (chat base + cutlass `HasServer`/`isRequestNotDeliveredError` + hoisted constants). Fix all import paths to fleet.
**Why this order:** stable public surface (only stdio+HTTP), but already drifted ~424 lines — consolidating first stops further divergence and validates the shared-package mechanics before the harder core.
**Test checkpoint:** chat's MCP client tests + cutlass's MCP loader tests (`mcp_loader.go` exercising `AddStdioServer` idempotency via `HasServer`) both green. Confirm cutlass's `matchesInt` string-id tolerance doesn't break chat's servers.

### P2 — `internal/agentcore` extraction
**Moves:** extract provider/cache/resilience/shared-orchestration primitives + fast.io guard/trimmer into `internal/agentcore` (§6.2); parameterize env-prefix and the `checkRepeatedCall` noun.
**Why now:** the central consolidation goal; depends on P1's merged MCP client for `mcpTool` wrapping.
**Test checkpoint:** new `agentcore` unit tests (lift chat's `cache_test`, cutlass's `resilience_test`/`orchestration_test`, the byte-identical `mcp_fastio_response_test`). `openrouterCost` and `checkRepeatedCall` parity tests against both prior behaviors.

### P3 — Two drivers behind the seam
**Moves:** reconstruct `internal/agent/interactive` (chat: `session.go`/`finalize.go`/`overflow.go`/`summarize.go`) and `internal/agent/scheduled` (cutlass: `Execute()`/`session_log.go`/`verifier.go`/`mcp_loader.go`) as thin drivers over `agentcore` + their three seam impls. Move `internal/sandbox` (chat) and `internal/tools` (merged; bash/python parameterized by Executor seam). Move `internal/captainslog` (scheduled only).
**Why now:** drivers can't exist before the core they consume.
**Test checkpoint:** chat agent suite (`session_test`, `finalize_test`, `overflow_test`, `prompt_test`, `roster_test`, `mcp_optin_test`, `native_optin_test`) + cutlass agent suite (`agent_test`, `orchestration_test`, `cache_test`, `resilience_test`, `compaction_integration_test`, `execute_integration_test`, `tools_integration_test`) all green. **Sandbox**: `sandbox_test`, `sandbox_hardened_test`, `workspace_same_path_test`.

### P4 — Python MCP servers deduped
**Moves:** consolidate `/mcp` per §5 ledger (cutlass DSP servers win; pubmatic + sendgrid merge; keep gamma/mailbux; one email_lint). Wire both drivers to the single `/mcp` dir.
**Why now:** independent of Go driver work; can run partly in parallel with P3.
**Test checkpoint:** consolidated pytest (`test_sendgrid_*`, `test_ses_s3_email`, `test_xandr_reporting`, `test_pubmatic_*`, `test_medianet_*`, `test_triplelift_reporting`, `test_mcp_integration`) green; `@pytest.mark.expensive` skipped. **Per-tool diff verification for medianet/indexexchange** (close tool counts) to prove no chat-only tool dropped.

### P5 — Scheduler + backends into the one Mega Box process
**Moves:** move moc's `scheduler`/`storage`/`handlers`/`models`/`apikeys`/`db` into `internal/sched/*`; converge driver to `pgx`. Build `cmd/fleet/main.go` that boots the chat HTTP/SSE server, the scheduler HTTP server, **and** the 30s scheduler goroutine in one process; `cmd/cutlass` as the one-shot entrypoint; `cmd/fleet-admin` unified CLI.
**Why now:** needs both data layers (`store` from P3, `sched/db` here) and the agent drivers present.
**Test checkpoint:** moc unit tests (`handlers_test`, `db_test`, `storage_test`, `models_test`, `visibility_test`) against a test Postgres; chat `httpapi` tests; scheduler promotes a `scheduled→pending` task; lease-recovery + timeout loops fire. `sandbox-probe` exercises **both** `Pool.Take` and `Pool.TakeContainer` and is extended to cover a scheduled-agent task.

### P6 — Unified frontend (two views)
**Moves:** scaffold `/web` from chat; relocate chat under `/chat` segment; add `mocServer.ts` + `/api/orchestrator/*` proxy from `openapi.yaml`; re-port moc dashboard to React `/orchestrator`; unify `middleware.ts` on `elcano_auth`; converge eslint/vitest/markdown.
**Why now:** backends must be reachable (P5) before the proxy layer is validated.
**Test checkpoint:** vitest unit suites (chat's 20+ `.test.ts` + new orchestrator components) green; Playwright mocked run (`CHAT_MOCK_MODE=1`) covering `/chat` login+stream and `/orchestrator` task-create+list+log-view.

### P7 — End-to-end Mega Box (chat + sandbox + scheduling)
**Moves:** full deploy via unified `bootstrap.sh` (test **both** `--postgres=local` and `--postgres=external`, plus idempotent re-run + non-interactive mode). One systemd `fleet.target`.
**Why last:** validates the entire consolidation under real Podman + real Postgres + both views.
**Test checkpoint:** **Live E2E** — Playwright live suite (`test:e2e:live`, real OpenRouter) drives an interactive chat turn that runs `bash`+`run_python` inside a rootless-Podman sandbox container; the orchestrator view schedules a recurring task whose cron triggers, gets leased (simulated gig runner / direct cutlass invocation), runs the one-shot agent in its container, and reports `success` + logs back. Verify: SSE streaming, sandbox hardening flags, scheduler promotion, both DB pools, single `elcano_auth` session across both views.

---

## 10. Risks & open decisions for the user

Crisp questions where a human must choose:

1. **Module path casing — confirm CamelCase.** The fleet remote is already `git@github.com:ElcanoTek/fleet.git` and 3/4 repos use `ElcanoTek`, so I recommend **`github.com/ElcanoTek/fleet`**. Confirm? (This must be locked before any import is rewritten.)

2. **Single vs multi-module — confirm single.** I recommend **one `go.mod`** so `internal/agentcore` is a normal shared import, with **gig excluded** (separate repo, remote runners). Agree, or do you want gig folded in too?

3. **One DB vs two schemas — confirm two databases.** I recommend **one cluster, two databases (`chat` + `sched`)** because the two `users` tables are structurally incompatible. Only pick the two-schema fallback if your managed-Postgres plan **bills per database** — does it?

4. **External-Postgres role provisioning.** Default = **assume roles/databases are pre-provisioned** (no superuser handed to bootstrap). Do you want the opt-in path where you supply a superuser DSN and bootstrap auto-creates them?

5. **`sslmode` for external Postgres.** I'll default external mode to **`sslmode=require`**. Is `require` sufficient, or do you need `verify-full` (CA bundle) for your provider?

6. **Migration engines — leave both, or unify?** v1 recommendation: **leave chat's advisory-lock runner and moc's golang-migrate as-is** in separate databases. Do you want me to converge the scheduler onto chat's lighter runner (drops the `golang-migrate`+`iofs`+driver deps) in a later phase?

7. **Frontend shell route.** Interactive chat at **`/` (root)** with orchestrator at `/orchestrator`, or chat at `/chat` with `/` as a landing/switcher? (Both are zero-code-change for `page.tsx`/`page-client.tsx`.)

8. **moc password login.** If we standardize on the `elcano_auth` cookie, do we **drop moc's username/password Bearer path** entirely, or keep proxying it so non-Elcano operators still work?

9. **pubmatic MCP surface.** Recommendation: **expose BOTH** the prepared-deal (cutlass) and discovery (chat) tool sets. Or do you want to standardize on prepared-deal only (which drops chat's `pm_discover_dsps`/`pm_discover_dsp_buyer_map`/`pm_create_targeting` — confirm no live chat protocol invokes them)?

10. **Sandbox base image.** Standardize the one Containerfile on **`fedora:43` (pinned, reproducible, ~190MB)** or **`fedora-minimal` (~80MB, saves ~110MB/pull)**? And keep the registry tag `ghcr.io/elcanotek/sandbox:latest` or rebrand to `ghcr.io/elcanotek/fleet-sandbox`?

11. **Warm-pool sizing per workload.** Interactive chat wants a warm pool (e.g. Size=3) to hide cold-start; scheduled one-shot agents can cold-start (Size=0). Confirm bootstrap should let the operator set pool size per workload.

12. **lifeline — confirm external.** I recommend **keeping lifeline out of fleet** (per-developer OpenRouter-only tool, no fleet coupling, idempotent cross-agent installer). Confirm, or do you want it vendored in-tree?

13. **Agent run-loop ownership (design seam depth).** Recommendation: **keep the fantasy run-loop per-driver** (`RunTurn` vs `Execute` differ in stop conditions/prepare-steps/enforcement order); share only primitives. Or do you want the loop body itself pulled into `agentcore` behind hooks (more unification, higher risk)?

---

*All paths above are absolute under `/root`. Decisive facts confirmed against the real repos: fleet remote = `git@github.com:ElcanoTek/fleet.git` (CamelCase); chat agent imports `internal/sandbox` while cutlass uses direct `exec.CommandContext` with no sandbox import; chat `NewBashTool(sb *sandbox.Sandbox)` vs cutlass `NewBashTool()`; chat=`jackc/pgx/v5`, moc=`lib/pq`; both Go agents pin `charm.land/fantasy v0.31.0`; Go 1.26.4 (chat/cutlass), 1.25 (moc), 1.22 (gig); 8 chat migrations (forward-only) vs 13 moc up/down pairs; lifeline = standalone `lifeline.py` + `install.sh`.*
