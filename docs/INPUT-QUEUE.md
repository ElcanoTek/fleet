# Input queue & mid-turn steering (#785)

Submitting to a conversation whose turn is still running no longer implicitly
cancels that turn. The submission becomes a durable **queue row** (acknowledged
only after the insert commits), and explicit `POST /conversations/{id}/cancel`
stays the ONLY Stop. Queued follow-ups drain as ordinary separate turns;
steer-mode inputs are additionally offered to the running turn at its next
safe step boundary.

## Lifecycle

Every accepted input has a stable server id, the caller's `client_input_id`
idempotency key, and a state: `queued → running → completed` (drained as a
turn), or `queued → injected → completed` (steered into the running turn), or
`cancelled` (removed / covered by Stop). A re-POST of the same
`(conversation, input_id)` returns the existing item (200) instead of
duplicating it.

## API

- `POST /chat` gains `input_id` and `mode` (`queue` default, `steer`). While a
  turn runs it returns **202** `{queued:true, input:{...}}` (200 on idempotent
  replay) instead of an SSE stream. A steer submission with attachments
  downgrades to `queue` (steering is text-only).
- `GET /conversations/{id}/queue` — authoritative pending snapshot.
- `DELETE /conversations/{id}/queue/{inputID}` — remove while still queued
  (409 once it ran).
- `POST /conversations/{id}/queue/{inputID}/send-now` — promote to the head;
  a running turn is also offered it at the next boundary.
- `POST /conversations/{id}/cancel` gains `{"scope":"turn"|"all"}` — default
  **all**: Stop cancels the active turn AND every still-queued input. The
  `queue.updated` SSE event carries a full snapshot on every mutation, and
  `user.message` gains `{steered:true, input_id}` when a steer is accepted.

(These are chat-surface routes; `docs/openapi.yaml` documents the orchestrator
surface only, so the API contract lives here.)

## Steering semantics

Steer inputs inject through `agentcore.Deps.SteerSource` — a nil-safe seam the
interactive driver backs with the queue (scheduled/evals leave it nil). The
`steeringStep` PrepareStep runs FIRST in the chain, so an injected user
message is budget-accounted by the #793 context guard and cache-marked like
any other history, and injection happens strictly BETWEEN provider steps —
never mid-tool. The durable `queued → injected` flip commits BEFORE the model
can see the text; if the flip loses a race with remove/Stop the injection is
refused. The injected message rides the run transcript (`user_text` entry,
deduplicated across resilience re-drives) into the #798 terminal commit, so
it persists exactly once; its queue row completes only with that commit. A
turn that ends before injecting leaves the row queued — the durable fallback
runs it as the next turn. User input is never silently dropped.

## Failure handling

A drained turn settles its rows against the #798 durable record at turn end:
the row completes ONLY if the turn's user entry committed; a pre-commit
failure (model resolution, DB blip) re-queues it — a 202-acknowledged input
is never silently lost. Uncommitted injected steers equally return to the
queue when their turn hard-errors. Re-queued rows (including
concurrency-cap refusals and transient launch failures) get a bounded
delayed re-kick so the queue self-heals without waiting for the next
submission. Stop scope=all records a per-conversation epoch before sweeping,
so a row in claim-limbo (claimed by a racing drain, invisible to the sweep)
still cannot launch after Stop. The `input_id` idempotency key is honored on
the direct path too: a retry that lands after the conversation went idle
returns the accepted item instead of running a duplicate turn.

## Drain & recovery

There is no per-conversation goroutine: every enqueue and every turn
completion kicks `maybeDrainQueue`, which claims the FIFO head with a
DB-atomic `FOR UPDATE SKIP LOCKED` and launches it through the same
`startTurn` path as a direct submission (all governance — sandbox, policy,
ceilings, #798 commit gating — unchanged). `registerTurn` refuses while a
turn runs; a lost race un-claims the row. At boot, `RecoverInputQueue`
resolves rows claimed by a dead process against the #798 durable record
(committed → completed, otherwise back to queued) and deliberately does NOT
auto-drain: a restart must not start unattended LLM spend — restored inputs
are visible in the queue UI and run on send-now or the next submission.

## Honest scope (deferred)

- Steer with attachments (downgrades to queue); steering for scheduled runs.
- Queue reorder beyond send-now promotion; drag-and-drop in the UI.
- Cross-restart idempotency for immediate submissions that never carried an
  `input_id` (clients that send one are covered).
- An injected-then-crashed steer re-queues (its turn's history never
  committed); if the turn's tools had side effects before the crash, the
  #798 recovery markers carry the evidence — the re-run is visible, not
  silent.
- Rate-limit RPM accounting applies at enqueue time; drained turns re-check
  only the per-user concurrency cap.
