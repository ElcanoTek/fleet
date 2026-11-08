# ADR-0104: One scheduler — chat schedules run on the sched engine

- **Status:** Accepted. Landed in two steps: part (a) (#2076) added the
  engine half (sched migration 076, claim / recovery / settle, the tick phase
  and wake-ups, the deliverer seam); part (b) (#2077) moved the rows
  (migration 077), wired the chat server in and deleted the chat loop.
- **Date:** 2026-11-07
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0005](0005-separate-chat-and-sched-databases.md) (already
  superseded by [ADR-0098](0098-one-database-two-schemas.md) for the database
  layout) for its scheduler part: the scheduling engine is no longer one of two
- **Issue:** #2049 ("One persistence plane"), milestone M5

## Context

ADR-0005 split fleet into two persistence domains, and each grew its own
scheduler:

- **The sched engine** (`internal/sched/scheduler`, `internal/sched/db`): one
  tick loop (`Scheduler.runLoop`, 30s by default) that promotes due tasks,
  recovers expired leases, reconciles recurrence and prunes run history.
  Every task occurrence holds a lease (`lease_owner`, `lease_expires_at`)
  while it is in flight, and a sweep re-queues the ones whose holder died.
- **Chat schedules** (in-chat patrols and timers, chat migration 086): rows in
  `chat.chat_schedules`, fired by a second loop — a 10s goroutine in
  `cmd/fleet` calling `httpapi.SweepDueChatSchedules` — plus per-schedule
  `time.AfterFunc` wake-ups for short timers. No lease (the claim and the
  enqueue were one chat transaction), no run history beyond `run_count` and
  `last_run_at`, no retention.

Two loops meant two sets of shutdown/drain gates, two claim models, two
answers to "what fired, when, and what happened". M5 of #2049 asks for one
tick loop, one lease model and one run history.

Since ADR-0098 both stores live in one database (schemas `chat` and
`sched`), so one connection can reach both — which is what makes a fold
possible without a cross-database protocol.

## Decision

**The sched engine is the one scheduler. Chat schedules become its rows and
are fired by it; the chat server is only the deliverer of a fire.**

- **Storage.** The table moves to the scheduler's schema:
  `sched.chat_schedules` (same columns, ids and values) plus the engine's
  lease columns `lease_owner` / `lease_expires_at` and the fire a lease is
  for (`fire_id`, `fire_trigger`, `fire_run_number`, `fire_failures`), a
  definition version (`def_version`, bumped by every edit of the prompt,
  cadence or kind; recorded on the fire at its claim as `fire_def_version`:
  a fire whose definition was edited since is superseded — it delivers,
  counts and records nothing, and the next pass fires the new definition),
  and
  `sched.chat_schedule_runs`, the run history (one row per fire that ran
  or was turned away: `schedule`/`manual` trigger, outcome `queued` /
  `busy` / `gone`, the run number of a queued run, the input-queue row,
  due and fire times). Sched migration
  076 creates both; 077 **moves** every row from `chat.chat_schedules` in one
  transaction, then drops the chat table.
- **One scheduler, one tick for chat schedules.** The `Scheduler` owns two
  goroutines, started and stopped together: the task loop (`runLoop`:
  promotion, lease recovery, the task sweeps) and the chat schedule loop
  (`runChatLoop`), so nothing on the task path — a promotion blocked on a row
  lock, a long sweep — can delay a chat fire. Every task-path storage call
  in the scheduler runs on its lifetime context (cancelled by Stop) with a
  per-sweep timeout, so none can hang the task loop or Stop either. The chat
  loop runs a pass on every tick interval, once as soon as it starts, and on
  a wake-up it arms for the
  earliest work due before the next tick — an active schedule's slot, or a
  pending fire's retry (lease expiry) — (and that the chat server
  requests when a schedule is created, edited or resumed; a request is kept
  when it is within one tick interval or five minutes, whichever is longer).
  A pass only claims — at most as many fires as a bounded pool of delivery
  workers (4) has free — and returns to its select at once; each worker
  delivers its fire under its own bounded context, records the result under
  a fresh one (so a delivery that ran out of time still leaves its settle,
  or its failure and backoff, on record), and asks for the next pass, which
  claims more while any is due. A claimed row stays out of the claims until
  something durable changes it, so a pass that finds nothing claimable asks
  for nothing: the backlog drains without a deadline or cap, and never
  spins. Promotion on the task loop likewise reruns at once past its
  per-pass row cap only when the pass promoted something. The
  daily maintenance (run-history prunes, log archival) runs in its own
  single-flight goroutine, so its long sweeps never delay a tick or a
  wake-up; Stop cancels and waits for it. The
  chat-side sweeper goroutine and its `AfterFunc` timers are deleted.
- **One lease model.** A fire is claim → delivery → settle. The claim
  (`db.ClaimDueChatSchedules`, `FOR UPDATE SKIP LOCKED`) writes a per-claim
  token into `lease_owner` with an expiry, as a task lease does; the chat
  server's `store.DeliverChatScheduleFire` enqueues the turn in a chat
  transaction that re-checks the lease and status under lock, keyed by the
  idempotency key `chat-patrol:<id>:<run>:<s|m>-<fire_id>` (the kind in the key keeps 077's
  history backfill right on a re-upgrade after a rollback); `db.SettleChatScheduleFire`
  advances the row (run count, next slot or completion), appends the run
  history row and releases the lease — only while the row still carries the
  claim's token. A fire whose process died mid-way is not a new due fire:
  when its lease expires the recovery pass (`db.RecoverChatScheduleFires`)
  re-claims the SAME fire — same `fire_id` (part of the idempotency key) and
  run number, under a new token — whatever the schedule's status or due
  time, so its re-delivery finds its own queue row (never a second turn) and
  the dead claim's late settle is refused. A delivery that errors keeps its
  fire and lease, the lease pushed out by a backoff (30s doubling to 30m), so
  a failing schedule is retried by recovery and never crowds out the healthy
  ones. A fire the chat server defers (draining, shutting down) is kept the
  same way with a 10s retry — not counted, not dropped, and delivered once
  when turns are taken again, a Run now on a paused schedule included. A
  refusal a retry would not change — a Run now on a conversation whose input
  queue is full — is the opposite: the fire ends at once, uncounted and
  unrecorded, and the caller is told (429); nothing runs it later. A
  scheduled fire meeting a full queue is `busy` (its slot skipped), as the
  chat sweeper always did. "Run now" takes the same path (`Scheduler.RunChatScheduleNow`): only
  its claim runs on the caller's context; delivery and settle run on a
  detached, bounded one, and a second Run now while a fire is pending is
  refused rather than minting a new fire for the same run; so is a Run now
  while the schedule's patrol turn is still queued or running (of two racing
  Run nows exactly one runs, the other gets `ErrChatScheduleFiring`, a 409).
- **One run history.** `sched.chat_schedule_runs` records the fires that
  ran (`queued`, with the run's number) and the fires turned away
  (`busy`: the slot was skipped; `gone`: the conversation was deleted or the
  owner lost access and the schedule was cancelled), the latter with no run
  number, so no row ever claims a number a later run takes. A fire that did
  nothing adds no row: a Run now that coalesces onto a patrol turn already
  queued or running (answered 409 — the earlier fire's row covers that turn),
  a Run now refused on a full queue (429), a stale or deferred fire. The
  history is pruned by the scheduler's daily retention sweep under the same
  policy as task runs (`FLEET_RUN_LOG_RETENTION_DAYS`,
  `FLEET_KEEP_RUNS_PER_TASK`). The migration seeds each schedule's history
  from the patrol turns still in `chat_input_queue`.
- **What stays on the chat side, and why.** Editing a schedule's definition
  (create / update / pause / cancel, the five-per-conversation cap, cancelling
  its queued turns) stays in `internal/store/chat_schedules.go`, writing
  `sched.chat_schedules` by its qualified name, because each edit must be
  atomic with the conversation's lock and access check and the input queue —
  all chat tables. Delivery (busy/coalesce rules, approvals, queue limits)
  stays there for the same reason. The chat store also *reads* the table for
  conversation lifecycle (TTL exemption, multiplayer reconcile, approval
  resume, account erasure). The scheduler never reads a chat table: it reaches
  the chat side only through `scheduler.ChatScheduleDeliverer`, which the chat
  HTTP server implements.
- **No external change.** The HTTP routes (`/conversations/{id}/schedules…`,
  `/patrols`), their response shapes, the `chat_patrol` tool, the SSE
  `patrol.updated` event and the SPA are unchanged; the orchestrator's task
  API is untouched (chat schedules are not tasks — see Alternatives).

### Upgrade

Migration 077 runs at the first boot of the binary that cuts over (the chat store always
migrates before the scheduler, so on every install the chat table exists by
then — empty on a fresh one). It locks `chat.chat_schedules` ACCESS EXCLUSIVE,
copies, and drops it in the same transaction:

- **No double fire.** The old table cannot fire after the copy (it is gone in
  the same commit); an outgoing process mid-update either committed its fire
  first — its advanced `next_run_at` / `run_count` is what is copied — or
  blocks on the lock and then fails on the dropped table. Run numbers continue
  from the copied `run_count`, so the new engine's idempotency keys never
  collide with turns the old engine queued, and a patrol turn still queued or
  running across the restart makes the first new fire `busy`, exactly as the
  old claim did.
- **No skipped slot.** `next_run_at` is copied as it stands; a slot that fell
  due during the restart fires on the new engine's first pass (once, not once
  per missed slot — the old behaviour).
- **Idempotent and reversible.** Every statement is `IF NOT EXISTS` /
  `ON CONFLICT DO NOTHING`, and the block is skipped once the chat table is
  gone. The down migration recreates `chat.chat_schedules` as 086/096 left it
  and copies the rows back (run history has no home there and is dropped).

## Enforcement

- `internal/sched/db/chat_schedules_migration_test.go` seeds old-shape rows
  and patrol turns on a database at sched 076 and pins the move (077) (column for
  column), the dropped chat table, the history backfill, re-run idempotency,
  the down/up round trip, and the first post-upgrade pass firing a due slot
  once with the next run number.
- `internal/sched/db/chat_schedules_test.go` pins claim/settle (queued, busy,
  gone, max runs, timers), recovery of an expired fire as itself (scheduled,
  and manual on a paused schedule), the failure backoff, the refused stale
  settle, Run now's claim errors and the retention prune.
- `internal/sched/scheduler/chat_schedules_engine_test.go` pins the pass
  against a fake deliverer: a failing batch does not starve the rows behind
  it, a backlog drains in one pass, the wake horizon follows the tick, Run now
  settles after its caller's context ends, and a pass runs at startup.
- `internal/sched/scheduler/chat_schedules_test.go` runs the tick phase, Run
  now and the between-tick wake-up against both real stores in one database.
- `internal/store/chat_schedules_test.go` pins that a pause or cancel
  between delivery and settle revokes the fire (no run counted, a paused
  timer stays paused); `internal/store/chat_schedules_cascade_test.go` pins
  that every hard-delete path takes a conversation's schedules and history
  with it, and that account erasure works on a chat-only database.
- There is no other ticker for chat schedules to drift back into: the cmd
  sweeper and `SweepDueChatSchedules` are deleted.

## Consequences

- One loop owns every timed thing the box fires; a crash mid-fire is
  recovered by the same lease expiry as a task, and patrols get a run history
  with retention.
- Chat schedules fire at their due time (the armed wake-up) instead of on a
  10s poll.
- The chat store depends on the scheduler's schema existing (its queries name
  `sched.chat_schedules`), so the chat store's own test template now migrates
  both stores (it is the same template `internal/testdb/fleetdb` builds).
- `conversation_id` is no longer a foreign key (the conversation is in the
  other schema). Sched migration 077 restores the cascade: a statement-level
  `AFTER DELETE` trigger on `chat.conversations` deletes the deleted rows'
  schedules (their history follows by sched's FK), so every hard-delete path
  — present and future — is covered by one mechanism; it is a no-op while
  the sched table does not exist. On a database the scheduler initialised
  first (no chat schema yet when 077 ran), `fleet serve` installs the same
  trigger once both schemas exist — retried in the background with backoff
  (5s doubling to 5m) for the life of the process if it fails — and the
  install removes any schedule orphaned in between
  (`db.EnsureChatScheduleCascade`, `db.KeepChatScheduleCascade`). It is created by the
  sched side, not a chat migration, so 077's down drops it and a rollback never moves the chat
  schema's version (the previous release refuses a newer chat schema). Account erasure deletes a user's schedules
  the same way (and tolerates a chat-only database, for the chat-only CLI
  verbs).
- Pausing or cancelling a schedule revokes a fire in flight (its lease and
  fire are cleared under the row lock its settle needs): a turn the user
  stopped is not counted as a run. A patrol turn already *running* when its
  schedule is paused finishes but is not counted.
- Restoring a chat-schema dump taken before this release recreates an inert
  `chat.chat_schedules` that nothing reads; restore the sched dump of the
  same moment for the schedules.
- The run history is not yet exposed in the API or UI.

## Alternatives considered

- **Make each patrol fire a `tasks` occurrence** (a chat-turn task kind
  claimed by the runner). One table for everything, but the tasks table *is*
  the orchestrator API's surface (`GET /tasks`, the Operations Center, the
  manifest service's task keys): every list, count, budget, SLA and digest
  query would have to learn to exclude patrols, and a missed one changes an
  external API. A patrol turn is also not a sandboxed run: it executes in the
  chat's own turn loop. Rejected.
- **Fold sched into chat** (the reverse). The chat side has no leases,
  recovery or history to fold into. Rejected.
- **Keep the table in `chat` and only call the chat sweep from the sched
  tick.** One loop, but still two claim models and no history; it moves a
  goroutine without unifying anything. Rejected.
- **Move the definition edits into the scheduler too** (sched code locking
  chat conversations). It would make the scheduler read and lock chat
  tables, the coupling this ADR keeps one-way. Rejected.
