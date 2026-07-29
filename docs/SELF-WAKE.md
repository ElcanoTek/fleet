# Self-wake: sleep / wake_on_event

## The gap

A scheduled run that needed to continue *later* had no good shape. "Check
the results in an hour" meant either staying alive (holding a sandbox and a
worker slot for an hour) or ending and losing the thread — cron recurrence
fires on a calendar, not on "when the thing I started finishes", and
`schedule_task` creates a *different* task behind an approval card. A
standing watch ("act when the deploy webhook fires") couldn't be expressed
at all except as blind cron polling.

## What shipped

Two SCHEDULED-only tools, the timer/event counterpart of `ask` (#510), with
the exact same park-and-requeue lifecycle — the run **ends** (sandbox and
lease released, partial transcript persisted first), the task parks in the
new non-terminal status `paused_awaiting_wake`, and the scheduler re-queues
it as a **fresh run**. Nothing suspends in memory; ADR-0024's "the loop is
not serializable" stands.

- **`sleep`** `{seconds | until, note}` — park until a deadline. Minimum 1
  minute (the sweep ticks every ~30s; sleeping is not polling), maximum 30
  days.
- **`wake_on_event`** `{event, timeout_seconds?, note}` — park until
  `POST /tasks/{id}/wake` fires the matching event key, or the timeout
  passes (default 7 days, max 30). **Every wake has a deadline**: an event
  wait that times out is woken and told the event never arrived — nothing
  waits forever, and no separate expiry sweep exists.

**The note is required.** A woken run is a fresh run; the note — the agent's
message to its future self — plus the wake reason is what crosses the gap.
Both are injected into the resumed run's prompt ("## Woken — Continue") the
same way a human answer is after `ask`, and cleared only at a terminal
transition (#582 semantics: a retryable failure of a woken run still injects
them). Captain's-log task memory (`remember`/`recall`) works across wake
cycles as across any runs.

### Mechanics

- The wake transition is one indexed sweep in the existing 30s scheduler
  tick (`WakeDueTasks`): due parked tasks flip to `pending` with
  `scheduled_for = now()`, `wake_reason` recording why ("the sleep timer
  fired" / "timed out waiting for event …" / "event … fired: <note>").
- `POST /tasks/{task_id}/wake {event, note?}` wakes an event-parked task
  early. The key must match the one the task waits for — a caller can never
  wake an arbitrary sleeping task — and the endpoint requires operator
  (cancel) permission, mirroring `POST /tasks/{id}/resume`.
- **Cycle cap:** `wake_cycles` counts every park over the task's lifetime
  (incremented inside the lease-guarded park write); past 100 the runner
  refuses the park and the tool tells the model to finish normally. A
  recurring task never accumulates cycles across occurrences — each
  occurrence is a new task row.
- **Cost accumulates:** each wake cycle re-runs the same task id; transcript
  cost adds up on the same task exactly as ask-resume cycles do, and
  per-attempt transcripts are preserved by run-log history
  (`docs/RUN-LOG-HISTORY.md`).
- Registration follows the ask contract: the tools exist only when the
  runner installed a wake handler on the run context — interactive chat
  never sees them.
- A parked task holds no lease, is not "active" for serialization keys, can
  be cancelled by an operator like any non-terminal task, and is excluded
  from retention (non-terminal rows are never pruned; the deadline
  guarantees it leaves the parked state).

## Deliberately deferred / honest scope

- Inbound webhook triggers (ADR-0016/0027) still only SPAWN runs; wiring a
  trigger to `POST /tasks/{id}/wake` (event-driven standing watches without
  an operator in the loop) is a natural follow-on.
- No web UI affordance to fire an event or list sleeping tasks beyond the
  status badge and the ordinary task table; the endpoint + CLI filters
  (`--status paused_awaiting_wake`) are the operator surface.
- `wake_cycles` cap is a constant (100), not configurable.
- The stream's terminal frame for a parking run is `sleeping`; clients that
  don't know the status render it like any other terminal frame.
