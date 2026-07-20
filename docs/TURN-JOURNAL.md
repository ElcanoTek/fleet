# Durable turn journal & commit-gated terminal success (#798)

Interactive turns preserve the causal chain
**user input → tool-call intent → tool outcome → assistant conclusion**
durably, before terminal success is advertised. The invariant is recorded in
[ADR-0039](adr/0039-durable-turn-journal.md); this page is the shipped design.

## Event taxonomy → storage

| Taxonomy event | Durable record | Idempotency key |
|---|---|---|
| turn started | existing `turns` row (`CreateTurn`) | `turn_id` PK |
| user committed | `messages` row `(turn_id, turn_seq=1)`, pre-provider | `messages_turn_seq` unique index |
| tool intent | `turn_journal` kind=`tool_intent`, pre-dispatch, synchronous | `(turn_id,'tool_intent',call_id)` |
| tool result | `turn_journal` kind=`tool_result`, post-governance, synchronous | `(turn_id,'tool_result',call_id)` |
| assistant text / reasoning | not journaled mid-turn (no side effect; regenerable); recovered best-effort from full-fidelity `turn_events`; durable at finalization | — |
| turn finalized | `turns.history_committed_at` + `messages` rows `(turn_id, turn_seq>=2)` in one tx | commit guard + unique index |

The SSE `turn_events` ledger stays a delivery/view layer. Its 4 KB
`tool.result` preview is never the source of truth for model replay: the
journal carries the exact governed, bounded model-visible bytes from the
[#793 output boundary](TOOL-OUTPUT-BOUNDARY.md).

## Write path

- `agentcore.Deps.TurnJournal` (nil-safe seam, driver-supplied data — the
  scheduled driver leaves it nil; its session log is the durable record).
  `policyGuardedTool.Run` and `mcpTool.Run` journal the intent AFTER the
  pre-tool gates (a blocked call never journals) and BEFORE dispatch; a failed
  intent write refuses dispatch fail-closed. The outcome is journaled right
  after `boundModelVisibleToolResponse`, so journal, model, policy audit, and
  stream sink all carry identical bytes. Disclosure-bridge wrappers do not
  journal — the deferred tool journals once under its real name.
- `httpapi.turnJournalWriter` persists records with a bounded retry (3
  attempts, fresh 5s contexts). After an exhausted write it is **degraded**:
  every later intent fails (no further side effects) and terminal success is
  refused.
- `Manager.RunTurn` commits the user entry via `TurnInput.CommitUser` before
  the first provider call, and gates `turn.completed` / `turn.cancelled` on
  `TurnInput.CommitTerminal` (→ `Store.CommitTurnHistory`: FOR-UPDATE guard on
  `history_committed_at`, retry-after-ambiguous-commit absorbed via
  `ErrTurnHistoryCommitted`). A failure returns `ErrHistoryCommitFailed` → a
  visible `turn.error`; `RecordTurn`/metrics/title/sweeps are skipped, and
  `inferTerminalStatus` records the turn as `error`.

## Startup recovery

`Store.RecoverStrandedTurns` runs at boot (`cmd/fleet` `run()`, after
`SetSearchEnabled`, before serving). Per `running` turn, in one transaction:
rebuild entries from the journal (authoritative for calls/results) interleaved
with `turn_events` text/reasoning (`turn.retry` drops the abandoned attempt's
text; steered `user.message` events project as user entries exactly once by
`input_id` — recovery stamps `history_committed_at`, which completes the
steer's queue row as "durably in history", so the text must actually be
there, #826); pair every call with a result — synthesizing an explicit
unknown-outcome error (`synthesized=TRUE` journal marker, model-visible
"verify before retrying" warning) when the real result never landed, or a
"did NOT execute" marker when the journal was active and the call has no
intent row (the barrier proves it was blocked before dispatch); append a
model-visible interruption marker + a cancelled `turn_summary`; project with
provenance; flip the turn to `error` with `recovered_at` and a synthetic
`turn.error` SSE frame. Idempotent across repeated/interrupted recoveries.

Two guarded special cases: a turn whose history committed but whose process
died before `FinishTurn` is flipped to `completed` (nothing to project — the
answer is whole; without the flip it would stay a zombie `running` row
forever), and a stale turn recovered only on a LATER boot — after the
conversation already moved on to newer turns — terminates WITHOUT projecting
(late projection would append old content after newer exchanges; the journal
keeps the evidence).

## What is deliberately NOT here (honest scope)

- **No late re-projection of alive-but-unpersisted turns.** A turn whose
  terminal commit failed while the process stayed up showed the user a visible
  error; projecting it later — after newer messages — would corrupt global
  message ordering. The journal keeps the evidence for manual reconstruction.
- **No per-step usage checkpointing.** Recovered turns carry a zero-usage
  cancelled `turn_summary` and no `turn_metrics` row.
- **No per-tool idempotency classification.** Every resultless call gets the
  same unknown-outcome reconciliation marker; a future classification could
  auto-retry known-idempotent calls.
- **Scheduled runs unchanged** (session-log persistence, per the issue).
- **Recovered assistant text is best-effort** (async `turn_events` deltas; the
  `turn.retry` rollback is approximate). Tool calls/results are exact.
- Approval placeholder overrides are untouched: post-turn resolutions append
  provenance-less rows under the same `call_id`, and replay's
  authoritative-result selection already prefers them (covers synthesized
  recovery results the same way).

## Operator notes

Postgres does one extra commit per turn (user entry) and two synchronous
INSERTs per tool call. On a WAN-remote Postgres that adds ~2×RTT per tool
call; text streaming is unaffected.
