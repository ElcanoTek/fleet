# Admin-managed workspace feature settings

Settings → Admin → **Feature settings** lets an admin see, change, and reset a
curated set of workspace-wide feature toggles from the web UI — the settings
that previously existed only as env flags, were documented unevenly, and in
practice went unused because nobody could find them. Every change applies
**live** (the next turn, run, or tool call picks it up); nothing in the panel
needs a restart, ever.

## How it works

- **Precedence: admin override → env var → built-in default.** The env vars
  keep working exactly as before and act as the deployment defaults. An admin
  override (a `workspace_settings` row, chat-DB migration 035) wins until it is
  **Reset**, which deletes the row and reverts to the env-derived value. The
  panel shows each setting's provenance ("Customized" vs "Server default"),
  the default it reverts to, and the env var it comes from.
- **Validated before persisted.** Values are checked against the server-side
  registry (`internal/settings`) — type, enum membership, integer bounds —
  before the row is written, so a bad value can never poison the store or the
  running process (the same discipline as config hot-reload, #286). An
  override that stops validating after an upgrade (e.g. a tightened bound) is
  not served — the default is — but it stays visible in the panel as an
  **"Ignored override"** with its attribution, so it can be Reset rather than
  lingering invisibly. The registry bounds constrain what an admin may write;
  an env-var default is normalized by the owning runtime before it reaches the
  panel. In particular, `FLEET_MAX_TOOL_OUTPUT_BYTES` is clamped to 1 KiB–128 KiB
  and zero/negative values select the safe 64 KiB default; no setting can
  disable the hard model-visible boundary.
- **Applied live.** At boot (after the store is ready) and after every admin
  edit, the effective value is pushed into the running system: the PII
  redactor is hot-swapped (`agentcore.SetPIIRedactor`), the two agentcore
  knobs update atomic holders read per turn/tool call, and the config-backed
  toggles go through mutex-guarded `Live*` getters on the shared
  `config.Config` (the same lock the #286 reload uses; the two mechanisms
  cover disjoint fields and cannot fight). With **no override set**, the two
  agentcore knobs keep reading their env var live per use (the holder is
  cleared, not pinned), so env-file edits + `reload-config` behave exactly as
  they did before this feature. Failure honesty is per-layer: an edit whose
  apply hook fails is **rolled back** (Set) or **restored** (Reset) so the DB
  can never disagree with the running system across a restart; a stored value
  that fails to apply at boot (e.g. a rampart engine whose service URL
  disappeared) keeps the panel up and marks that row ("saved but NOT in
  effect", `apply_error`) so the admin can fix or Reset it from the UI —
  and fixing a dependency **auto-heals** dependent keys without a reboot.
  Only a boot that cannot even load the override rows degrades the panel to
  "unavailable" (501), because then it could not render truthfully at all.
- **API**: admin-gated `GET /admin/settings`,
  `PUT /admin/settings/{key}` (`{"value":"…"}`), `DELETE /admin/settings/{key}`
  (reset) on the chat server; web proxies under `/api/admin/settings`. Every
  write — set and reset — is audit-logged (key, validated value, admin
  identity).
- **No secrets, by construction.** The registry admits feature toggles and
  numeric bounds only. Secret-bearing config (SMTP passwords, webhook signing
  keys) is deliberately not admin-settable — see "What stays env-only" below.

## The registry

| Setting key | Kind | Env var default | What it controls |
| --- | --- | --- | --- |
| `pii_redaction_mode` | `off`/`observe`/`redact`/`block` | `FLEET_PII_REDACTION_ENABLED` + `FLEET_PII_REDACTION_MODE` | Optional PII pass over tool output ([PII-REDACTION.md](PII-REDACTION.md)) |
| `pii_redaction_engine` | `pattern`/`rampart` | `FLEET_PII_REDACTION_ENGINE` | Detector: built-in regexes or the Rampart ML service ([PII-REDACTION.md](PII-REDACTION.md)) |
| `pii_rampart_url` | http(s) URL (or empty) | `FLEET_PII_RAMPART_URL` | Rampart detection service endpoint |
| `guardrail_mode` | `off`/`observe`/`block` | `FLEET_GUARDRAIL_MODE` | Host-side untrusted-ingress screening ([GUARDRAILS.md](GUARDRAILS.md)) |
| `guardrail_url` | http(s) URL (or empty) | `FLEET_GUARDRAIL_URL` | Prompt-injection detector endpoint |
| `tool_disclosure_threshold` | int 1–100000 | `FLEET_TOOL_DISCLOSURE_THRESHOLD` | Roster size that triggers BM25 tool disclosure ([TOOL-DISCLOSURE.md](TOOL-DISCLOSURE.md)) |
| `max_tool_output_bytes` | int 1024–128 KiB, or 0 = 64 KiB default | `FLEET_MAX_TOOL_OUTPUT_BYTES` | Operational per-tool result cap inside the non-disableable 128 KiB model-visible boundary ([TOOL-OUTPUT-BOUNDARY.md](TOOL-OUTPUT-BOUNDARY.md)) |
| `phone_a_friend_enabled` | bool | `FLEET_PHONE_A_FRIEND_ENABLED` | One-time super-LLM review of scheduled runs ([AGENT-RUNTIME.md](AGENT-RUNTIME.md)) |
| `subagents_enabled` | bool | `FLEET_SUBAGENTS_ENABLED` | Fleet-wide **kill switch** for sub-agent delegation — default **on** (#1043); composes AND with per-task `allow_delegation` ([SUBAGENTS.md](SUBAGENTS.md)) |
| `memory_autoindex_enabled` | bool | `FLEET_MEMORY_AUTOINDEX_ENABLED` | Post-turn memory auto-indexer ([MEMORY.md](MEMORY.md)) |
| `error_analysis_enabled` | bool | `FLEET_ERROR_ANALYSIS_ENABLED` | Post-failure LLM diagnosis of failed tasks (#317) |
| `auto_title_enabled` | bool | `FLEET_AUTO_TITLE` | LLM-generated conversation titles (#302) |
| `connector_recommendations_enabled` | bool | `FLEET_CONNECTOR_RECOMMENDATIONS_ENABLED` | Reactive connector nudges (#512) |
| `context_handles_enabled` | bool | `FLEET_CONTEXT_HANDLES_ENABLED` | Composer `@url:`/`@file:` handles ([CONTEXT-HANDLES.md](CONTEXT-HANDLES.md)) |

The registry contract: **every entry must be live-applicable** — its consumer
re-reads the value per turn, per run, or per tool call. A setting that is bound
at boot may not join this registry (the panel would have to lie about a change
requiring a restart); it stays in the env file.

## What stays env-only, and why

The #638-era audit that produced this feature inventoried every env-configured
knob. These deliberately did **not** move into the panel:

- **Boot-bound plumbing** — listener addresses, DB DSNs/pools, TLS,
  `FLEET_MAX_CONCURRENT_AGENTS` (sizes the admission semaphore + sandbox warm
  pool), sandbox image/resources, the Python REPL pool
  (`FLEET_PYTHON_REPL_*`), search FTS upkeep (`FLEET_SEARCH_ENABLED`),
  self-improving memory
  (`FLEET_SELF_IMPROVE_ENABLED`), memory graph, log/retention/cleanup
  schedules. These bind into listeners, pools, or wiring at startup; a restart
  is the honest unit of change. (The web UI's old concurrency-cap card, which
  called a `PUT /concurrency` endpoint fleet never served, was removed in the
  same change — it could only ever error.)
- **Secret-bearing notification config** — now has its own admin surface with
  exactly that sealed-secretbox, write-only treatment: Settings → Admin →
  **Notifications** ([NOTIFICATIONS.md](NOTIFICATIONS.md), `notify_settings`
  migration 036). It is a separate slice from this registry because it holds
  secrets. VAPID keys (Web Push) stay env-only — deployment identity material
  bound at boot.
- **Already hot-reloadable ceilings** — `FLEET_MAX_COST_USD`,
  `FLEET_MAX_TOTAL_TOKENS`, `FLEET_MAX_ITERATIONS`, temperatures ride the
  existing env-file reload ([CONFIG-RELOAD.md](CONFIG-RELOAD.md), SIGUSR2 /
  `POST /admin/reload-config`). Folding them into the panel is a candidate
  follow-on but was kept out of this slice to avoid two write paths to the
  same fields.
- **Fine-grained tuning** — sub-agent depth/children/budget-fraction, model
  slug overrides (`FLEET_PHONE_A_FRIEND_MODEL`, `FLEET_MEMORY_MODEL`,
  `FLEET_ERROR_ANALYSIS_MODEL`), compaction thresholds, task-memory caps.
  Env-only for now; the master toggles above are the administrable surface.

## Honest scope / deferred

- The panel is **workspace-global** — no per-user, per-team, or
  per-conversation overrides.
- Settings changes apply to the *next* turn/run/tool call; in-flight work is
  never interrupted (this is a feature, not a limitation).
- The registry lives server-side; the panel's labels/help copy live in the
  web app (`FeatureSettingsPanel.tsx`). A registry key added server-side
  before the panel learns its copy still renders (under "Other") from its raw
  key — new settings never silently vanish.

See [`adr/0030-admin-workspace-settings.md`](adr/0030-admin-workspace-settings.md)
for the decision record.
