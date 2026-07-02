# Building on fleet: the API as your automation substrate

fleet isn't only a chat app with a scheduler — it's a **programmable agent
runtime with an HTTP API**. Anything that can send an HTTP request (a cron job,
a CI pipeline, a Slack bot, another fleet task, your own CLI) can enqueue
governed agent work, watch it run, and consume machine-readable results. Every
job you kick off still flows through the one governed core: mandatory sandbox,
cost/token ceilings, audit, host-side credentials.

The full API surface is specified in [`openapi.yaml`](openapi.yaml) (kept
honest by a route-parity CI test) and versioned per
[`api-versioning.md`](api-versioning.md) (the `/v1` prefix + `X-Fleet-API-Version`).
This page is the practical tour.

## 1. Mint a scoped API key

Typed keys (#190) carry their access class in the token itself —
`fleet_task_…` can create/view tasks, `fleet_readonly_…` can only read,
`fleet_webhook_…` can only fire its named triggers:

```sh
fleet sched apikey create ci-bot --type task
# → prints fleet_task_<base58> exactly once — store it in your caller's secrets
```

Optionally cap what it may submit (`--max-priority 30`) or rate-limit it.
Authenticate with either header: `X-API-Key: <key>` or
`Authorization: Bearer <key>`.

## 2. Kick off a job

```sh
curl -s -X POST "$FLEET/tasks" \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{
    "prompt": "Pull yesterday'\''s signup numbers from the warehouse and write a 5-line summary.",
    "model": "anthropic/claude-opus-4-8",
    "priority": 30,
    "mcp_selection": {"servers": ["warehouse"]},
    "recurrence": "0 8 * * 1-5"
  }'
# → the Task JSON, including its id
```

Useful create-time fields (see `TaskCreate` in openapi.yaml for all of them):

| field | what it buys you |
| --- | --- |
| `recurrence` | 5-field cron; omit for one-shot (`scheduled_for` for a delayed one-shot) |
| `priority` | 0–100, lower = more urgent; anti-starvation promotion built in |
| `mcp_selection` | which catalog connectors this task may use (least privilege) |
| `output_schema` | draft-07 JSON Schema → the run's final answer is validated and stored as machine-readable JSON |
| `sandbox_limits` | per-task memory/cpu/pids override, bounded by operator ceilings |
| `allow_network` | unseal the sandbox's egress for this task (default sealed) |
| `carry_context` | recurring runs start with the prior run's final answer |
| `expected_duration_minutes` | SLA warn/fail tracking |
| `allow_task_creation` | let the run enqueue follow-up tasks (agents scheduling agents) |
| `max_retries` / `retry_policy` | transient-failure retries with backoff |

Batch up to 100 at once with `POST /tasks/batch` (`atomic: true` for
all-or-nothing), estimate cost first with `POST /tasks/estimate`, and version
your job definitions in git via `fleet task export` / `import` (JSON or YAML).

## 3. Watch it run, consume the result

```sh
curl -s "$FLEET/tasks/$ID" -H "X-API-Key: $KEY"            # status / metadata
curl -s "$FLEET/tasks/$ID/stream" -H "X-API-Key: $KEY"     # live SSE: tool calls, results, progress
curl -s "$FLEET/tasks/$ID/output" -H "X-API-Key: $KEY"     # the schema-validated JSON (when output_schema was set)
curl -s "$FLEET/tasks/$ID/artifacts" -H "X-API-Key: $KEY"  # files the run produced
curl -s "$FLEET/tasks/$ID/error-analysis" -H "X-API-Key: $KEY"  # LLM failure diagnosis on terminal failure
```

`output_schema` + `GET /tasks/{id}/output` is the composition primitive: your
caller gets **validated JSON**, not prose to re-parse — pipe it into the next
system (or into the next task).

## 4. Let events kick off work (instead of polling)

- **Inbound webhook → task**: register a trigger (`fleet sched trigger create`),
  then any system that can POST with the HMAC secret (GitHub, Stripe, CI) fires
  `POST /triggers/{slug}` and a task run spawns from its template. Email-driven
  triggers ride the same machinery (`docs/EVENT-TRIGGERS.md`).
- **Inbound webhook → conversation**: a signed `POST /webhooks/{slug}` on the
  chat server starts an interactive conversation from a template
  (`docs/WEBHOOKS.md`).
- **Outbound notify**: task completion/failure fires the configured notifier —
  webhook (HMAC-signed, `docs/WEBHOOK-SIGNING.md`), email, and browser Web Push
  (`docs/PUSH-NOTIFICATIONS.md`) — so your ecosystem reacts instead of polling.
- **Human-in-the-loop**: a run that calls `ask` parks as `paused_awaiting_input`
  (holding no sandbox) and `GET /tasks/paused` + `POST /tasks/{id}/resume
  {answer}` are the queue API (`docs/ASK-NOTIFY.md`).

## 5. Patterns that fall out

- **Nightly report bot**: cron `recurrence` + `output_schema` + a webhook
  notifier posting the validated JSON to Slack.
- **CI agent**: a `fleet_task_` key in CI; on red main, POST a task whose prompt
  carries the failing job URL; consume `/output` to decide whether to open an
  issue automatically.
- **Fan-out pipelines**: a parent task with `allow_task_creation` enqueues
  per-item children; or use the dataset agent (`docs/DATASETS.md`) for
  row-by-row work with human-approved write-backs.
- **Your own frontend**: everything the bundled Operations Center does goes
  through this same API — build your own.

## Boundaries to respect

- Keys are secrets: scoped keys can't mint keys, and `fleet sched apikey list`
  never re-prints a raw key. Rotate with `apikey rotate`.
- A task's power is bounded at create time (`mcp_selection`, `allow_network`,
  priority caps on the key) — prefer narrow grants and let the audit trail stay
  boring.
- The governed core is not bypassable via the API: ceilings, sandbox, and
  approval gates apply to API-created work exactly as to chat.
