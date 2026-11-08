# ADR-0111: Remove zero-usage experimental subsystems; enable memory auto-indexing by default

- **Status:** Accepted (supersedes ADR-0023, ADR-0029; amends ADR-0019)
- **Date:** 2026-11-08
- **Deciders:** fleet maintainers

## Context

Several experimental or speculative subsystems accumulated in the codebase with
zero production usage across the estate, while adding HTTP routes, CLI flags,
agent tools, background tickers, and configuration settings:

1. **Derived memory knowledge graph (ADR-0029, `#523`)** — `memory_entities` /
   `memory_relations`, `GET /memories/graph`, `POST /memories/{id}/extract-graph`,
   `as_of_valid`/`as_of_learned` on `GET /memories`, the web UI's Graph tab, and
   `FLEET_MEMORY_GRAPH_ENABLED` / `FLEET_MEMORY_GRAPH_MODEL`. Flat, typed,
   provenanced memory records (ADR-0019) and human-confirmed contradiction
   retirement already answer what users and agents need.
2. **Task feedback / learned-instruction distillation (ADR-0023, `#516`)** —
   thumbs-up/down ratings and critique distillation (`task_feedback` /
   `task_learned_instructions`, `POST /tasks/{id}/feedback`,
   `GET /tasks/{id}/learned-instructions*`, `FLEET_SELF_IMPROVE_ENABLED`).
   Operators edit prompts directly when a recurring task misbehaves rather than
   down-voting three runs.
3. **Dataset / table batch agent (`#514`)** — `internal/datasets`, `/datasets*`
   endpoints, and the Operations Center Datasets tab.
4. **Scheduled evals & replay harness (`#502`)** — `internal/evals`,
   `fleet eval`, `config/default/evals`, and `eval_runs`.
5. **Daily owner digest (`docs/OWNER-DIGEST.md`)** — `internal/ownerdigest` and
   `owner_digest_enabled` / `owner_digest_hour_utc` (while keeping per-task
   completion notifications and browser Web Push).
6. **Captain's Log per-task KV memory & Operations Center admin notes (`#198`,
   `#285`)** — `remember`/`recall` task-KV tools (`task_memories`,
   `FLEET_TASK_MEMORY_MAX_KEYS`, `FLEET_TASK_MEMORY_MAX_VALUE_BYTES`) and
   `read_notes`/`propose_note_update` (`notes`/`note_proposals`, `/notes*`,
   `fleet notes`, `fleet sched task-memories`).
7. **Scheduled task human-in-the-loop `ask` / `notify` tools (`#510`)** —
   `internal/tools/ask_notify.go` and `/tasks/paused` + `/tasks/{id}/resume`
   (while keeping `sleep` / `wake_on_event` / `POST /tasks/{id}/wake` and A2A's
   `TASK_STATE_INPUT_REQUIRED` state transitions).
8. **Phone-a-friend super-LLM review (`#175`)** — `internal/agent/reviewer.go`
   and `FLEET_PHONE_A_FRIEND_ENABLED` / `FLEET_PHONE_A_FRIEND_MODEL`.

Meanwhile, post-turn user memory suggestions (`FLEET_MEMORY_AUTOINDEX_ENABLED` /
`memory_autoindex_enabled`) remain human-gated (surfacing reviewable proposals
rather than mutating state silently) and should be on by default on every
installation.

## Decision

1. **Remove the eight zero-usage subsystems above** (packages, HTTP routes,
   OpenAPI operations, CLI subcommands, agent tools, and web UI surfaces).
   Existing database tables/columns added by historical migrations are left in
   place as inert schema history so the linear migration ledger (`ADR-0107`)
   remains intact.
2. **Retire the corresponding environment variables in `envreg`'s
   `removedSettings`:**
   - `FLEET_MEMORY_GRAPH_ENABLED`, `FLEET_MEMORY_GRAPH_MODEL`
   - `FLEET_SELF_IMPROVE_ENABLED`
   - `FLEET_PHONE_A_FRIEND_ENABLED`, `FLEET_PHONE_A_FRIEND_MODEL`
   - `FLEET_TASK_MEMORY_MAX_KEYS`, `FLEET_TASK_MEMORY_MAX_VALUE_BYTES`
   (along with their legacy `CHAT_`/`CUTLASS_` spellings) so any leftover line
   in `fleet.env` or the process environment reads as unset and is reported by
   boot, `fleet doctor`, `fleet config explain`, and
   `fleet validate-config --retired-only`.
3. **Enable `FLEET_MEMORY_AUTOINDEX_ENABLED` by default (`true`).** It remains
   an approval-gated proposal loop; operators can still disable
   `memory_autoindex_enabled` live in Settings → Admin → Features (or via
   `FLEET_MEMORY_AUTOINDEX_ENABLED=false`).

## Consequences

- User memories (`memories` table, ADR-0019) and bundle skills/prompts remain
  the clear, supported knowledge and customization mechanisms.
- Leftover retired settings on existing boxes are flagged by `fleet doctor` with
  a clear removal message.
- New and existing deployments get post-turn memory suggestions out of the box
  without manual env-file opt-in.
