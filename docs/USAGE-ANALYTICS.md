# Usage analytics & budgets (#601)

Spend/usage analytics over the metering fleet already persists — who (user /
API key), what (project / model), and when (day / week) drove cost and tokens
— plus, in part 2, per-principal rolling budgets enforced at task-create over
that same metering. This page records what shipped, what deviated from the
issue, and what was deliberately deferred.

- **Part 1 — the read model**: `GET /admin/usage` + the Operations Center
  Usage panel. Strictly read-only.
- **Part 2 — rolling budgets**: `{scope: user|key, window:
  day|week|month, soft/hard bounds in dollars AND tokens}`, enforced by one
  shared gate at every task-create path, with a once-per-window soft alert
  through the existing notifier. See "Part 2" below.

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
- Chat usage survives conversation deletion. `turn_metrics` intentionally has
  no cascading conversation foreign key (chat migration 038): deleting or
  expiring transcript content must not erase already-incurred accounting.
  Model/project attribution falls into the documented empty bucket once the
  joined conversation is gone; user, cost, token, and time meters remain.

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

## CSV export

`GET /admin/usage?format=csv` returns the current report as a `text/csv`
attachment (`fleet-usage-<group_by>-<YYYYMMDD>.csv`): one row per bucket with
the group key, cost columns, and token/iteration/turn counts, plus a trailing
`TOTAL` row. Same admin gate + `group_by`/`from`/`to` params as the JSON form
(`?format=json` is the default). The Operations Center Usage panel has a
**Download CSV** button that hits it with the panel's current filters.

## Deferred (deliberately, part 1)

- Chat-side per-model attribution finer than the conversation's model override
  (`turn_metrics` does not record the per-turn model actually used).

---

# Part 2 — per-principal rolling budgets with alerts

## What shipped

### The budget model (one table, all scopes)

A budget is `{scope: user|key, principal_id, window: day|week|month,
soft_usd?, hard_usd?, soft_tokens?, hard_tokens?}` — persisted in a single new
sched-DB table `budgets` (migration 052), unique on `(scope, principal_id,
window)`. All bounds are optional individually; at least one is required, and
soft may not exceed hard per measure.

**Persistence choice, recorded honestly:** the issue offered "a small table
(sched migration) or fields on the scoped API key (like `MaxPriority`, #190) —
pick per scope". One table covers **user and key** (and could hold a
future project scope), including `key`,
because the API key's existing per-key caps (`MaxCostPerDayUSD` /
`MaxCostPerMonthUSD`) are a *separate accounting path* — a JSON-file
accumulator fed by task-completion callbacks — while #601's budgets must be
computed from the part-1 usage read model (the persisted `task_iterations` +
`turn_metrics` metering), and must support week windows, token bounds, and
soft alerts uniformly across scopes. The legacy per-key caps remain and
compose: both gates only ever narrow.

- Window boundaries are UTC calendar windows (day = UTC day; week = Monday
  00:00 UTC, matching `date_trunc('week')` in the part-1 queries; month = UTC
  calendar month), so enforcement and the usage report agree.
- Spend is **never accumulated on the budget row**. Every check recomputes the
  principal's current-window spend from the part-1 aggregations
  (`Storage.TaskUsage` + the chat `turn_metrics` seam) — no second accounting
  path exists to drift. Budget principals are the part-1 bucket keys: sched
  username (user), API key id (key), chat project id (project).

### Enforcement — one gate, every create path

`internal/sched/budget.Enforcer.CheckCreate` is the single shared gate,
mirroring the `priorityCapError` shared-helper discipline:

| Create path | Where the gate runs | Principals |
|---|---|---|
| `POST /tasks` | `handlers.budgetCapError` | user (bearer/cookie) or key |
| `POST /tasks/batch` | same helper, once per request (the budget bounds the principal, not a task) | user or key |
| chat `schedule_task` (approval seam) | `taskSchedulerProvider` in `cmd/fleet`, before `EnqueueTask` | user = the approving chat user's email (`TaskScheduleRequest.RequestedBy`) |

- **Hard bound reached** → the create is refused with **402 Payment Required**
  plus `Retry-After` at the window rollover (the chat path surfaces the same
  error as the schedule_task failure text). Distinct from the 429 the per-key
  rate/spending caps use.
- **Soft bound crossed** → the create is admitted and **exactly one** notify
  alert fires per window crossing, through the SAME `internal/notify` pipeline
  task-completion notifications use (email/webhook, plus Web Push routed to
  the user for user-scope budgets). The crossing marker
  (`budgets.soft_alert_window_start`) is claimed with a conditional UPDATE
  **before** delivery, so concurrent creates race to one alert and a restart
  cannot re-alert. Editing a budget resets the marker (re-arms the alert).
- **Tokens as well as dollars** (#289 honest scope): native-provider runs
  accrue $0 without a pricing override, so budgets bound prompt+completion
  tokens too — a token hard bound refuses even at $0 spend.
- **Fail-safe composition with the live global ceilings (#286)**: at every
  check the effective hard bounds are `min(budget, FLEET_MAX_COST_USD /
  FLEET_MAX_TOTAL_TOKENS)` read through the live hot-reload accessors, so a
  budget row can never be more permissive than the box-wide ceiling and a
  reload is honored on the very next create. Soft bounds are not clamped (they
  only time an alert). The per-run enforcement inside `agentcore` is untouched
  — this gate is purely additive; nil/absent budget is byte-for-byte today's
  behavior (one indexed SELECT, no aggregation, nothing refused).
- An unverifiable budget (aggregation/DB error) **fails closed**: the create
  path returns 500 rather than admitting work unchecked.

### Admin surface

- `GET /admin/budgets` — every budget with its live evaluation (current window
  `[start, end)`, spend in both measures, the effective clamped hard bounds,
  whether this window's soft alert fired).
- `POST /admin/budgets` — upsert on `(scope, principal_id, window)`.
- `DELETE /admin/budgets/{budget_id}`.
- All documented in `docs/openapi.yaml` (schemas `Budget`, `BudgetCreate`,
  `BudgetStatus`; bound in the route/schema parity tests), registered like
  `/admin/usage` behind `AdminOrUserAuthMiddleware` with the admin gate
  enforced in-handler (#458).
- The Operations Center Usage panel renders the budget list **read-only**
  under the usage report (spend, bounds with the clamp made visible, and an
  ok/alerted/exhausted status), with the token-vs-dollar coverage caveat
  spelled out. Budget create/delete is deliberately API-only for now.

## Honest scope (part 2)

- **`scope=project` is rejected on write.** Tasks carry no project dimension
  and no task-create path resolves one, so a project budget could only be
  recorded and never enforced. Leftover rows still list. Re-introduce the
  scope when chat `schedule_task` threads `project_id`.
- **Admin-key submissions are not budget-gated.** The admin API key carries
  neither a user nor a scoped key — it is the box operator, whose bound is the
  global ceiling. Likewise the in-process spawn paths (`create_task` from a
  running task, webhook/email trigger spawns, rerun/clone) are not gated in
  this PR — deliberate deferral; the three issue-named create paths are.
- **User identity is string equality with the part-1 buckets**: sched
  `users.username` on the task meter, chat email on the chat meter. On a
  standard deployment both are the email and one budget bounds both meters;
  where they differ, a budget bounds whichever meter matches its
  `principal_id`. Also: tasks scheduled *from chat* are attributed to no sched
  user (only the `source-chat` tag), so their iteration spend lands in the
  unattributed bucket — the chat user's budget gates the *creation* of such
  tasks but their run cost does not accrue against it. No cross-DB identity
  map was invented.
- **Enforcement is at task-create, not run-start.** A task admitted under the
  window's remaining budget may still run (and cost) after the window rolls
  over or the budget is exhausted by concurrent work; the per-run global
  ceilings bound each run as before. The issue's "and/or at run-start" option
  is deferred.
- **The global-ceiling clamp compares a per-run ceiling to a per-window
  bound.** `FLEET_MAX_COST_USD` / `FLEET_MAX_TOTAL_TOKENS` cap a single run in
  the runtime; the budget gate additionally treats them as an upper bound on
  any per-window hard budget (min-composition), per the issue's "never a way
  to exceed the global one". A hard budget set above the global ceiling is
  therefore effectively the ceiling value per window; `GET /admin/budgets` and
  the UI surface the clamped value so the narrowing is visible, not silent.

## Deferred (deliberately, part 2)

- Project-scope enforcement (above), run-start enforcement, and gating the
  in-process spawn/rerun paths.
- Budget create/edit/delete UI (the panel is read-only; CRUD is API-only).
- Slack (or other) alert channels — the alert rides the existing notify
  pipeline; new channels belong to that seam.
- Per-budget alert recipients: alerts go to the deployment-wide
  email/webhook channels (plus Web Push to the user for user-scope budgets).

# Part 3 — the adoption view (exec per-user audit)

## What shipped

`GET /admin/usage/adoption` + the Operations Center **Adoption** tab: the
executive answer to "who is actually using the agents, how often, and is that
growing" — a different question from part 1's "where did the spend go", which
is why it is a separate read model rather than a seventh `group_by`.

### The read model (still no new accounting path, no new tables)

The same two meters as part 1, aggregated on **(user, UTC day)** at once:

- `internal/sched/db/usage.go` — `Database.TaskUsageByUserDay(ctx, from, to)`
  over `task_iterations ⋈ tasks ⟕ users` (same provenance rules as
  `TaskUsage`: deleted creators degrade to their UUID, orphan rows land under
  the empty user, every iteration counts).
- `internal/store/usage.go` — `Store.UsageByUserDay(ctx, from, to)` over
  `turn_metrics` alone (user + day live on the metric row, so chat-side
  attribution here never degrades on conversation deletion).
- Two new seams mirror `SetChatUsageProvider`:
  `SetChatUserDayUsageProvider` (per-user-per-day chat metering) and
  `SetChatAccountsProvider` (the provisioned-account roster from the chat
  store's `users` table — emails and created-at only, never credentials).
  Wired in `cmd/fleet/main.go`; when nil the report covers tasks only /
  omits the seat denominator, and `sources` says so.

The handler (`internal/sched/handlers/adoption.go`) queries **both windows** —
[from, to) and the equal-length window immediately before — and merges into
`models.AdoptionReport`:

- `days`: the full UTC day axis, so every per-user `daily_tokens` series and
  the `daily` trend are index-aligned with no client-side date math.
- `users`: per-user merged totals (cost split task/chat, tokens, cached,
  iterations/turns), `active_days`, `last_active`, previous-window
  comparators, and the per-day token sparkline series. Sorted **token-volume
  first** (deliberately unlike part 1's cost-first sort): tokens are the
  coverage-independent meter (#289), so the leaderboard doesn't reshuffle
  when pricing config changes.
- `daily`: per-day cost/tokens/actions/active-user counts (the two trend
  charts).
- `inactive_users`: provisioned seats with no metered activity in the window
  — churned **and** never-started, listed explicitly so "who hasn't adopted"
  is a first-class answer.
- `totals`: active/prev-active/new-active headcounts, the seat denominator,
  and current-vs-previous cost/token totals.

The unattributed (empty) user is listed and totaled — spend never disappears
— but never counts toward the adoption headcounts.

### The endpoint

`GET /admin/usage/adoption?from=&to=&format=` on the orchestrator API,
documented in `docs/openapi.yaml` (schemas `AdoptionReport`, `AdoptionUser`,
`AdoptionSeat`, `AdoptionDay`, `AdoptionTotals`, all bound in the `cmd/fleet`
drift tests). Same parameter contract and admin posture as `/admin/usage`:
trailing-30-day default, `to` exclusive, 366-day clamp, registered behind
`AdminOrUserAuthMiddleware` with `PermissionAdmin` enforced in-handler.
`format=csv` downloads the per-user leaderboard with a TOTAL row.

### The Operations Center panel

`web/src/app/orchestrator/AdoptionPanel.tsx`, an admin-only **Adoption** tab
beside Usage: KPI tiles with previous-period delta chips (active users with
the seat denominator + adoption rate, new-this-period, tokens, spend), two
daily small multiples (tokens per day, active users per day — two measures,
two charts, never a dual axis), the per-user leaderboard (rank, sparkline,
tokens + delta, active-day ratio, engagement tier, cost, turns/iterations,
last active), and the "Not yet active" seat roster. The dashboard tabs now
honor a `?tab=` deep link (`/orchestrator?tab=adoption`), which the Settings
admin pages link to.

Engagement tiers are a **client-side presentation bucket** (power ≥ 50% of
window days active, regular ≥ 20%, light below) — not a server concept, not
persisted, and trivially retuned.

## Honest scope (part 3)

- **Token volume is an adoption signal, not a performance grade.** The
  report's `note` says exactly that, verbatim, in the UI. Someone burning
  tokens is *using* the tools, not necessarily producing more; the view is
  built for adoption auditing, and the caveat travels with the data.
- Dollar figures inherit the part-1 pricing caveat (#289); token totals are
  complete regardless — which is also why the leaderboard sorts on tokens.
- A user active in the previous window but not the current one appears in
  `inactive_users` (if provisioned) and in the `prev_*` totals, but has no
  per-user row — per-user churn detail beyond that is deferred.
- The seat denominator is the chat store's provisioned users. Sched-only
  principals (API keys, sched users with no chat account) still appear as
  active users when they spend, but are not seats — the adoption rate is
  "active users / provisioned chat accounts".
- Settings → Admin → Users/Overview remain **all-time, chat-only** account
  administration numbers fed by `GET /api/admin/stats`; they are now labeled
  "Chat spend" and cross-link here rather than pretending to be the same
  report. The three surfaces deliberately answer different questions:
  account admin (Settings), spend accounting (Usage), adoption audit
  (Adoption).

## Deferred (deliberately, part 3)

- Per-user drill-down (click a row → that user's conversations/tasks).
- Team/department roll-ups (needs the chat `team_id` threaded through the
  roster seam).
- Scheduled adoption digests (email/webhook) — the notify pipeline is the
  right seam when wanted.
- Churn-specific analytics (streaks, days-since-last-active buckets).
