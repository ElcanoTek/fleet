-- 059_add_self_wake.up.sql — self-wake (docs/SELF-WAKE.md).
--
-- A scheduled run can suspend itself and schedule its own resumption: the
-- sleep tool parks the task in status 'paused_awaiting_wake' (sandbox + lease
-- released, exactly like the ask pause #510) and the scheduler's tick
-- re-queues it when the wake condition is met.
--
-- wake_at        — when the task wakes on its own. ALWAYS set on a parked
--                  task: a timer sleep sets it directly; an event wait sets
--                  it as the timeout deadline, so nothing waits forever and
--                  no separate expiry sweep is needed.
-- wake_event_key — non-empty when the task waits for a named event
--                  (POST /tasks/{id}/wake with the matching key wakes it
--                  early; wake_at firing first records a timeout instead).
-- wake_note      — the agent's message to its future self, written at park
--                  time and injected into the resumed run's prompt.
-- wake_reason    — why the task woke ("timer fired", "event …", "timed
--                  out …"), written by the wake transition and injected
--                  alongside the note. Runtime state, like pending_answer.
-- wake_cycles    — total times this task has parked for a wake; the runner
--                  refuses to park past the cycle cap so a confused agent
--                  can't sleep-loop forever.
--
-- All five are RUNTIME state (like pending_question/pending_answer /
-- error_analysis): read via taskColumns, written only by the dedicated wake
-- transitions, excluded from the task insert/upsert and the TaskToCreate
-- clone recipe so a definition write or a recurrence spawn can never carry
-- or clobber them.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS wake_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS wake_event_key TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS wake_note TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS wake_reason TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS wake_cycles INTEGER NOT NULL DEFAULT 0;

-- The wake sweep's due query: parked tasks by deadline.
CREATE INDEX IF NOT EXISTS idx_tasks_wake_due
    ON tasks(wake_at) WHERE status = 'paused_awaiting_wake';
