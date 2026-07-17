# ADR-0039: Durable turn journal gates interactive terminal success

Status: accepted

## Context

Interactive turns persisted canonical history only AFTER `Agent.RunTurn`
returned, and `turn.completed` was emitted before any canonical write. A crash
(or a failed `AppendHistory`) could therefore leave external tool side effects
— an email sent, a record created — with no durable record in the history the
next model turn replays: the model repeats work it already did, and a
"completed" answer disappears on reload (issue #798). The SSE `turn_events`
ledger cannot substitute: its `tool.result` payloads are 4 KB UI previews and
its writes are asynchronous.

## Decision

A durable agent harness must preserve the causal chain
**user input → tool-call intent → tool outcome → assistant conclusion**
before it advertises terminal success:

- **User entry commits first.** The user message is written to canonical
  history (with `(turn_id, turn_seq=1)` provenance) BEFORE the first provider
  or tool call. A store failure fails the turn with no side effects taken.
- **Intent before dispatch, fail closed.** Every tool route (native, loader,
  direct + deferred MCP, confirm_audit) journals its call intent through the
  `TurnJournal` seam BEFORE executing; a failed journal write refuses dispatch.
  No side effect without a durable record.
- **Governed outcome before the next provider step.** The journaled result is
  the exact governed, bounded model-visible bytes from the #793 boundary —
  byte-identical to what the model, policy audit, and stream sink receive.
  The SSE preview limit is never the persistence limit.
- **Terminal events gate on the canonical commit.** `turn.completed` /
  `turn.cancelled` are emitted only after `CommitTurnHistory` transactionally
  projects the transcript and stamps `turns.history_committed_at`. A commit
  failure (or a degraded journal) surfaces a visible `turn.error` — never a
  completed answer that vanishes.
- **Startup recovery projects, not just flips.** A turn left `running` by a
  crash is projected into ONE explicit interrupted turn: journal-authoritative
  tool calls paired with results, an unknown-outcome error synthesized (and
  marked `synthesized=TRUE` for reconciliation) for any call without a result,
  best-effort assistant text recovered from `turn_events`, and model-visible
  interruption markers. Projection is idempotent across repeated boots
  (status predicate + `history_committed_at` + a partial unique index on
  `(turn_id, turn_seq)`).

The seam stays driver-supplied data on `agentcore.Deps` (like `UsageReporter`)
— the trunk gains no Mode branch and no store dependency. The scheduled driver
leaves it nil: its session log is already the durable record.

## Consequences

- Interactive turns cost one extra transaction up-front (user commit) and two
  synchronous INSERTs per tool call. Tool calls are 100ms+ operations; text
  and reasoning deltas stay on the async `turn_events` path.
- A degraded journal (lost write after bounded retries) deliberately blocks
  further tool dispatch and the turn's success — availability is traded for
  never advertising unrecorded side effects.
- Post-crash, the next model turn sees an explicit unknown-outcome warning for
  in-flight calls instead of silently re-running them; uncertain side effects
  are reconciled, not repeated.
- An errored-but-alive turn whose terminal commit failed is NOT re-projected
  later (late projection after newer messages would corrupt ordering); the
  user saw the visible error and the journal keeps the evidence.

See [`docs/TURN-JOURNAL.md`](../TURN-JOURNAL.md) for the shipped design and
honest scope.
