# Usage analytics (#601, part 1)

Spend/usage analytics over the metering fleet already persists: who (user /
API key), what (project / model), and when (day / week) drove cost and tokens.
This page records what shipped, what deviated from the issue, and what was
deliberately deferred.

**Part 2 of #601 — per-principal rolling budgets with soft/hard limits and
alerts — is NOT part of this feature.** This is the read model only; nothing
here gates task creation or enforces any limit.

## What shipped

### The read model (no new accounting path, no new tables)

Aggregation happens strictly over rows the governed core already writes:

| Source | Table | Meters | Dimensions |
|---|---|---|---|
| Scheduled tasks (sched DB) | `task_iterations` ⋈ `tasks` (⟕ `users`) | `cost_usd`, `prompt_tokens`, `completion_tokens` per iteration | creator (`created_by` → username, UUID fallback for deleted users), API key (`created_by_key_id`), model, iteration `started_at` |
| Interactive chat (chat store DB) | `turn_metrics` (⟕ `conversations` ⟕ `projects`) | `cost_usd`, `prompt_tokens`, `completion_tokens`, `cached_tokens` per turn | user email, project (id + name), conversation model, turn `completed_at` |

- `internal/sched/db/usage.go` — `Database.TaskUsage(ctx, from, to, groupBy)`.
- `internal/store/usage.go` — `Store.UsageSummary(ctx, from, to, groupBy)`.
- Both key their SQL grouping expressions off a fixed map validated against a
  closed `group_by` set, so caller input never reaches SQL.
- Failed/cancelled iterations and cancelled chat turns **count** — the cost was
  still spent.

The two sources live in **separate databases**, so the merge happens in the
handler (`internal/sched/handlers/usage.go`), keyed on the bucket key, with
per-source subtotals (`task_cost_usd`/`chat_cost_usd`, `task_iterations`/
`chat_turns`) preserved so the report is honest about where each number came
from. The chat store reaches the orchestrator handler through a
`SetChatUsageProvider` seam wired in `cmd/fleet/main.go` (the two backends run
in one process); when the seam is nil the report covers tasks only and its
`sources` field says so.

### The endpoint

`GET /admin/usage?group_by=&from=&to=` on the orchestrator API, documented in
`docs/openapi.yaml` (schemas `UsageReport` + `UsageBucket`, both bound in the
`cmd/fleet` route/schema parity tests).

- `group_by`: `user | key | project | model | day | week` (default `user`).
- `from`/`to`: RFC 3339 or `YYYY-MM-DD`; default trailing 30 days; `to`
  exclusive; window clamped to 366 days (the SLA report's clamp-don't-error
  convention).
- Admin-only, but registered behind `AdminOrUserAuthMiddleware` with the gate
  enforced in-handler on `PermissionAdmin` — the `/sla-report` pattern (#458),
  because the Next proxy can never send the admin `X-API-Key`, and the report
  is global across principals so a non-admin member gets 403.
- The empty bucket key is meaningful, not an error: tasks have no project
  dimension, chat turns have no API-key dimension, some rows have no creator.

### The Operations Center panel

An admin-only **Usage** tab (`web/src/app/orchestrator/UsagePanel.tsx`,
proxied via `web/src/app/api/orchestrator/admin/usage/route.ts`): range
presets (7/30/90 days) + group-by + measure (cost/tokens) controls, KPI tiles
(total cost with the task/chat split, prompt/completion/cached tokens, work
units), a single-hue bar chart for entity groupings (tail folded into “Other”
past 11 bars) or a column time series for day/week, and a full table twin so
no value is gated behind the chart or hover. Built per the dataviz skill: one
validated hue per theme (`--usage-bar`; lightness band, chroma floor, ≥3:1
contrast checked with the palette validator for light AND dark), text in text
tokens, hairline solid gridlines, refetches hold the previous render at
reduced opacity.

## Honest scope

- **Dollar coverage depends on pricing configuration (#289).** Native-provider
  runs accrue `$0` cost unless a pricing override is configured. The endpoint
  therefore always returns token totals alongside dollars, every response
  carries a `note` saying exactly this, and the panel renders it verbatim and
  keeps token KPIs beside the cost KPI.
- **Cross-source principal identity is by string equality.** Task usage is
  keyed by the sched `users.username`; chat usage by the chat store's user
  email. On a standard deployment usernames are emails and the buckets merge;
  where they differ, the same person shows as two buckets. No cross-DB
  identity mapping was invented for this read model.
- **Dimensional coverage is asymmetric and shown as such**: `group_by=project`
  puts all task spend under the empty key (tasks have no project);
  `group_by=key` puts all chat spend under the empty key (chat has no API-key
  dimension); `cached_tokens` is chat-only (`task_iterations` does not persist
  a cached-token count).
- **No index was added** for the `task_iterations.started_at` /
  `turn_metrics.completed_at` range scans. At the platform's stated scale
  (10–20 users on one box) the aggregations return in milliseconds;
  `turn_metrics` already has `(user_email, completed_at)`. Revisit if the
  tables grow past that envelope.

## Deviations from the issue

- The issue sketched the endpoint under the bare admin-API-key middleware
  family (`/admin/stats`-style). It ships behind `AdminOrUserAuthMiddleware` +
  in-handler `PermissionAdmin` instead, because the issue also requires the
  Operations Center panel to render it and the dashboard proxy cannot present
  an admin API key (#458 precedent). Both the admin key and an admin-role
  session are accepted; the gate is not weaker, just reachable.

## Deferred (deliberately)

- **Part 2 of #601 entirely**: per-principal rolling budgets
  (`{scope, window, soft_usd, hard_usd}`), soft-limit notify alerts, hard-limit
  refusal at every create path, and the fail-safe composition with the live
  global ceilings (#286). Ships separately; #601 stays open for it.
- CSV export of the report.
- Chat-side per-model attribution finer than the conversation's model override
  (`turn_metrics` does not record the per-turn model actually used).
