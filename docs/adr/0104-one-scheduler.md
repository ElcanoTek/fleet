# ADR-0104: One scheduler — chat schedules run on the sched engine

- **Status:** Accepted
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
are fired by its tick loop; the chat server is only the deliverer of a fire.**

- **Storage.** The table moves to the scheduler's schema:
  `sched.chat_schedules` (same columns, ids and values) plus the engine's
  lease columns `lease_owner` / `lease_expires_at`, and
  `sched.chat_schedule_runs`, the run history (one row per settled fire:
  run number, `schedule`/`manual` trigger, outcome `queued` / `coalesced` /
  `busy` / `gone`, the input-queue row, due and fire times). Sched migration
  076 creates both; 077 **moves** every row from `chat.chat_schedules` in one
  transaction, then drops the chat table.
- **One tick loop.** `Scheduler.runLoop` runs a chat-schedule phase on every
  tick and on a wake-up the loop arms for the earliest active schedule due
  before the next tick (and that the chat server requests when a schedule is
  created, edited or resumed). The chat-side sweeper goroutine and its
  `AfterFunc` timers are deleted.
- **One lease model.** A fire is claim → delivery → settle. The claim
  (`db.ClaimDueChatSchedules`, `FOR UPDATE SKIP LOCKED`) writes a per-claim
  token into `lease_owner` with an expiry, as a task lease does; the chat
  server's `store.DeliverChatScheduleFire` enqueues the turn in a chat
  transaction that re-checks the lease and status under lock, keyed by the
  idempotency key `chat-patrol:<id>:<run>`; `db.SettleChatScheduleFire`
  advances the row (run count, next slot or completion), appends the run
  history row and releases the lease — only while the row still carries the
  claim's token. A fire whose process died mid-way is re-claimed when its
  lease expires with the same run number, so its re-delivery finds its own
  queue row (never a second turn), and the dead claim's late settle is
  refused. "Run now" takes the same path (`Scheduler.RunChatScheduleNow`).
- **One run history.** Every fire, scheduled or manual, settles into
  `sched.chat_schedule_runs`, pruned by the scheduler's daily retention sweep
  under the same policy as task runs (`FLEET_RUN_LOG_RETENTION_DAYS`,
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
  gone, max runs, timers), lease expiry and re-claim, the refused stale
  settle, Run now's claim errors and the retention prune.
- `internal/sched/scheduler/chat_schedules_test.go` runs the tick phase, Run
  now and the between-tick wake-up against both real stores in one database.
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
  other schema). A schedule whose conversation is hard-deleted is cancelled by
  its next fire (`gone`) instead of disappearing with it; a paused one stays
  as an inert row. Account erasure still deletes a user's schedules.
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
