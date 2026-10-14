# Per-call tool timing

## Why

The tool-call audit ledger (`tool_call_log`, #224) has had a `duration_ms`
column since it shipped, but nothing filled it: the history the ledger is
derived from carries no timing. So when a customer reported that the platform
"got slower", a turn's wall-clock time could not be split between waiting on
the model and running tools. Every row on the affected deployment had a NULL
duration.

## What shipped

- **Measurement.** The outermost tool dispatch wrapper
  (`panicContainedTool.Run` in `internal/agentcore`) stamps the call's start
  time on the round's `streamSink`, reached through the stream context. When
  the result arrives, `streamSink.onToolResult` computes the elapsed time. The
  first stamp for a call id wins, because the deferred-MCP bridge dispatches its
  inner tool under the same id, and the outer dispatch is what the user waited
  on.
- **Stream.** `tool.result` frames carry `duration_ms` (an integer number of
  milliseconds) when the call was dispatched. It is absent for a call that never
  reached dispatch: invalid input, an unknown tool, or a provider-executed tool.
  The recorded chat-stream contract (`testdata/contracts/chat-stream/`)
  normalizes the value to `0`. The web, terminal and ACP consumers ignore the
  field today.
- **Ledger.** The HTTP turn buffer keeps each `tool.result`'s duration by call
  id, separately from its event window (which can evict early frames on a long
  turn). `deriveToolCallEntries` writes it to `tool_call_log.duration_ms`, and
  `GET /conversations/{id}/audit` already returns the column when it is set.

## What it measures, and what it does not

- The time from dispatch to result **for the tool itself**: sandbox exec, MCP
  round trip, approval staging. The model's streaming time is excluded, because
  fantasy dispatches a step's tools only after that step's stream has ended.
- Sequential calls in one step are timed individually. Each starts when its own
  dispatch begins, not when the step's first tool started.
- `started_at` on a ledger row is still the **turn's** start, not the call's.
  Rows carry a per-call duration, not a per-call start.

## Deferred

- Only interactive chat turns write the ledger. Scheduled runs emit the same
  `tool.result` field into their session log observer but have no ledger table.
- The chat UI's execution-trail chips do not show the duration yet. Showing it
  there (and persisting it in the transcript, so a reloaded chat shows it too) is
  a separate, user-visible change that would also update the `/help` guides.
- Rows written before this change keep a NULL duration. No backfill is possible.
