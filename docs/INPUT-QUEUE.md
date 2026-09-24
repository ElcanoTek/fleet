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
duplicating it while that row remains within retention.

Terminal (`completed` / `cancelled`) rows are retained for 30 days by default,
then purged at boot and after turns. `FLEET_INPUT_QUEUE_RETENTION_DAYS` changes
that window; `0` disables the purge. The window is also the idempotency-key
retention guarantee: after a terminal row is purged, reusing its
`client_input_id` creates a new input. Non-terminal rows are never purged.

## API

- `POST /chat` gains `input_id` and `mode` (`queue` default, `steer`). While a
  turn runs it returns **202** `{queued:true, input:{...}}` (200 on idempotent
  replay) instead of an SSE stream. A steer submission with attachments
  downgrades to `queue` (steering is text-only). `input_id` and
  `submission_id` are limited to 256 bytes (400 beyond); the key is indexed.
- `GET /conversations/{id}/queue` — authoritative pending snapshot.
- `DELETE /conversations/{id}/queue/{inputID}` — remove while still queued
  (409 once it ran).
- `POST /conversations/{id}/queue/{inputID}/send-now` — promote to the head;
  a running turn is also offered it at the next boundary.
- `POST /conversations/{id}/cancel` gains `{"scope":"turn"|"all"}` — default
  **all**: Stop cancels the active turn AND every still-queued input. An
  optional `turn_id` (from `turn.started`) targets one turn: it is cancelled
  only while it is the running turn (`204`); once it has ended the request
  stops nothing and answers `409`. Either Stop, by `turn_id` or by `input_id`,
  confirms a cancel against the turn's own terminal frame, read as soon as it
  is emitted (a turn does post-turn work, such as auto-titling, before its
  buffer seals). `turn.completed` (the turn finished in the instant between
  the running check and the cancel) counts as finished (`409`), as does a turn that had already emitted a
  terminal frame of its own before the Stop (it failed, and post-turn work
  still holds its buffer open, so the cancel would stop nothing) or that
  fails on its own between the check and the cancel; only `turn.cancelled`
  after the cancel is a confirmed stop (`204`) — an engine that fails with
  the Stop's own cancellation (in preflight, say) is advertised
  `turn.cancelled`, not `turn.error`; no terminal frame within a few
  seconds answers `202`, sent but not yet confirmed, never assumed stopped, so a client stopping the turn it watched
  (`fleet acp`) can never cancel a successor, and learns that the turn
  finished on its own rather than taking the Stop for a cancellation. A targeted Stop is turn-scoped and never sweeps the
  queue. An optional `input_id` targets one input by its idempotency key,
  wherever it is (an input that had already finished — it ran, or its turn
  ended with only its settlement pending — stops nothing and answers `409`,
  like a `turn_id` Stop of an ended turn): a still-queued row is withdrawn, a running turn for it is
  cancelled (a drained row is cancelled too, unless its user entry had
  committed — by the Stop when the turn confirms it, and by the turn's own
  settlement when the confirmation comes too late for the `202`, or when
  the turn had already failed before the Stop but not yet settled — so it
  is never returned to the queue for a later drain to run; if that cancel
  write fails, the turn's rows are left unsettled for a background retry
  rather than settled without it. The Stop also stamps the key's pending row
  `stop_requested_at` before it cancels anything, so settlement and boot
  recovery cancel an uncommitted row it named even if this process dies
  before its in-memory record is used (a stamped queued row is never
  injected as a steer, and a drain or boot recovery cancels one rather than
  launch it); a Stop whose stamp fails still stops
  the turn but is not reported as landed), a steer already injected into a running turn is cancelled and so
  is the turn carrying it (the model cannot un-read it; if that turn had
  already ended, nothing is stopped, the Stop answers `409`, and the turn's
  own settlement records whether the steer ran; a stop the turn confirms too
  late for the `202` still has its settlement cancel an uncommitted steer
  rather than re-queue it), and a turn not
  registered yet (a direct claim still being prepared, a row the drain just
  claimed, or a submission still in transit) is refused when it tries to
  register. The mark and the registration check share one lock, so a keyed
  input is either cancelled or never launched; the mark is kept for 10 minutes
  (at most 4096 marks at once; `input_id` is limited to 256 bytes here too),
  a queued row is withdrawn in the database (at the Stop, or when it is
  inserted after the Stop) so it cannot outwait it, and a claimed input not yet
  bound to its turn (a direct claim, or a drained row still holding its
  `claim-` placeholder turn id) is cancelled in the database too, so its launch
  is refused (a direct submission answers `409`) even if the mark is gone by
  then. A row already bound to its turn is left to that turn's settlement,
  since the turn may have run. A Stop that finds no row for the key (its
  submission still in transit) takes the key with a `cancelled` row, so the
  late submission is answered "cancelled" and never runs, even once the mark is
  gone; that row is purged with the other terminal rows. A client whose answer was lost (`fleet acp`) stops its own input this way without knowing which state
  it reached. `POST /chat` names its turn on the `X-Fleet-Turn-Id` response
  header (beside `X-Fleet-Conversation-Id`), so the id is known before any
  frame.
- `input_id` is honoured on the **direct** path too (migrations 064 and 065):
  a submission that starts a turn directly claims its key with a
  `mode:"direct"` row in the same table and unique index, so a resend of the
  same key, while the turn runs or after it ends, is answered `200` with that
  row's acknowledgement (`state` `running` / `completed` / `cancelled`)
  instead of a second turn. Direct rows are never queue items: the queue
  listing, drain, Stop sweeps, remove and promote skip them. At turn end (and
  at boot recovery) they settle `completed` when the turn's user entry
  committed and `cancelled` otherwise (nothing ran, so a fresh key may be
  sent). Settlement at turn end runs on its own bounded context and is retried
  in the background if it fails, so a finished claim does not keep answering
  "running". A claim whose turn fails before it launches is settled
  `cancelled` (a resend is told it did not run, so a fresh key may be sent),
  never deleted: a concurrent resend may already have been told it is running.
  A claim that loses the race to another surface's turn is released, so its
  input can be queued instead; a release that cannot be confirmed (it may have
  committed with its acknowledgement lost) is never retried as a delete but
  settles the key `cancelled`, retried in the background, so a resend already
  told "running" always finds a record. A claim is bound to its
  turn before the turn runs; if that fails, the turn is dropped and the
  submission fails (`500`) rather than running with a claim a crash could not
  match to it. A submission that loses the race to a turn started from another
  surface is queued only after its claim is released, and fails (`503`, send
  again) if the release cannot be confirmed; if the queue then refuses it (full,
  or a store error), the key is settled `cancelled` so a resend already told
  "running" finds an outcome. A claim is an accepted input, so
  a Stop scope=all that begins after it was accepted covers it: the claim
  settles `cancelled` and the submission answers `409` without running. A
  resend is answered before the request touches the conversation, so a replay
  never re-applies the original request's model or un-archives it. A client
  that declares its keys unique per user (`"input_id_scope": "user"`, as
  `fleet acp` does) has every key looked up per user, under the per-(user,
  key) lock below from that lookup until the key is claimed or queued: a
  resend finds its input whichever conversation accepted it, a concurrent send
  of the key into another conversation finds the first one's row, and a first submission's resend after a
  response lost before any header finds the original conversation instead of
  starting a second one. Without that declaration the key stays
  conversation-scoped, so a client that numbers keys per conversation is never
  answered with another conversation's replay. Concurrent submissions of one
  user-unique key, first ones included, are serialized by that lock, so the
  second finds the first one's claim. The lock is
  in-process, like the inflight registry the Stop gate relies on: the control
  plane is single-replica by design (the Helm chart pins one replica). The
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
is never silently lost. An uncommitted injected steer returns to the queue
when its turn hard-errors ONLY if no tool intent was journaled after the
row's injection watermark (`injected_seq`, stamped at the durable
queued → injected flip); a post-injection intent proves the model dispatched
a tool with the steer in context, and #820 preserves those committed side
effects, so the row CANCELS instead of re-executing the instruction (#823 —
at most once, never a double-sent email; the drop is logged, and the queue
snapshot the client re-reads at stream end reflects it — the finishing turn's
own buffer is already sealed, so nothing is pushed over it). Intents carry the
proof because they are journaled pre-dispatch and a degraded journal refuses
dispatch outright — an
unjournaled post-injection side effect cannot exist. Re-queued rows (including
concurrency-cap refusals and transient launch failures) get a bounded
delayed re-kick so the queue self-heals without waiting for the next
submission. Every accepted row carries a process-wide acceptance sequence
(`accepted_seq`, migration 058; seeded from the table at boot so it never
repeats across restarts). Stop scope=all records the counter's value per
conversation at the instant it begins and sweeps only the rows at or below
it, so a follow-up submitted a moment after Stop — while the cancelled turn
is still finishing — keeps its acknowledgement and runs (#1477); a row in
claim-limbo (claimed by a racing drain, invisible to the sweep) is refused by
the same comparison, so the sweep and the launch gate agree on exactly one
swept set, and a stepped wall clock cannot move a row across that boundary.
While the sweep runs, the drain defers (a per-conversation interlock) and a
claim that slipped past it is refused at registration by a per-conversation
Stop generation. The `input_id` idempotency key is honored on
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
(committed → completed; injected with a post-watermark tool intent →
cancelled, same #823 predicate as turn-end settlement; otherwise back to
queued) and deliberately does NOT auto-drain: a restart must not start
unattended LLM spend — restored inputs are visible in the queue UI and run
on send-now or the next submission.

Position allocation and send-now promotion serialize on the conversation row,
and a partial unique index enforces one position per non-terminal item (including
`running` / `injected` rows that recovery may re-queue). Drain and snapshot
queries additionally order by `created_at, id`, so databases upgraded from the
pre-index schema retain deterministic behavior while migration 046 normalizes
any legacy ties.

## Getting a drained turn onto the screen

The drain is server-side and the client is not subscribed to it: the finishing
turn's event buffer is **sealed** before the drain is kicked (settlement needs
the #798 commit, which happens after the seal), so the settle-time
`queue.updated` has no subscribers, and the drained turn opens a brand-new
buffer nobody asked to attach to. Nothing about that is pushed. So the web
client goes looking, at the one moment it can learn about it — when a turn's
stream ends (a direct submission's POST stream and a reattach alike):

1. re-read `GET /conversations/{id}/queue`, the authoritative snapshot;
2. if a `queued` or `running` row remains, attach to the turn the drain
   started (the existing `/inflight` + `/stream` reattach) and stream it like
   any other turn; when it ends, start again for the row behind it;
3. if the queue emptied without ever attaching, the drained turn ran and
   finished faster than the client looked (or its retain buffer went) — the
   canonical transcript is in Postgres, so reload it rather than leave the
   exchange off the screen;
4. if the row is still `queued` when the bounded backoff runs out, stop. A
   restart leaves rows queued deliberately (recovery never auto-drains), so an
   accurate chip with a send-now button is the honest end state, not a poll
   that never terminates.

The chip strip itself is read from `GET /queue` whenever a conversation opens,
not only from `queue.updated` — otherwise a reload or a chat switch shows an
empty strip with inputs still queued, and recovery's "visible in the queue UI"
promise has no UI to keep it in. A drained turn's buffer is also seeded with
the snapshot alongside its conversation/turn metadata events, so attaching to
that turn is enough to see that its own row has moved `queued → running` (an
inert chip, no send-now/remove).

A direct submission the server queues (the mirror of the stale-busy race: the
client believed the conversation was idle) is answered with the JSON ack, not
a stream — the client classifies the response and shows a chip instead of
pumping the ack as SSE.

## Honest scope (deferred)

- Steer with attachments (downgrades to queue); steering for scheduled runs.
- Queue reorder beyond send-now promotion; drag-and-drop in the UI.
- Cross-restart idempotency for immediate submissions that never carried an
  `input_id` (clients that send one are covered).
- An injected steer whose failed turn dispatched a tool after the injection
  is CANCELLED, not re-run (#823): the watermark can prove a post-steer
  dispatch happened but not that the steer specifically caused it, so the
  safe side is dropping a resendable message over duplicating a side effect.
  The user resends; exactly-once delivery (committing the steered entry at
  injection time) would need a redesign of the #798 one-commit-per-turn
  projection contract and is deliberately out of scope.
- Rate-limit RPM accounting applies at enqueue time; drained turns re-check
  only the per-user concurrency cap.
