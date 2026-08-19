-- 064_add_task_paused_at.up.sql — when the task last entered a paused state (#1116).
--
-- The paused-task expiry sweep (#510) previously measured a paused task's age
-- from started_at, "acceptably conservative" for short runs — but a run that
-- executes for 2h BEFORE calling `ask` under a 60-minute window was expired on
-- the next 30s tick: a zero TTL, and the human never got to answer.
--
-- paused_at is stamped by the dedicated pause transitions — PauseTaskForQuestion
-- ('paused_awaiting_input', #510) and PauseTaskForWake ('paused_awaiting_wake',
-- docs/SELF-WAKE.md) — and ExpirePausedTasks now filters on it, so the ask
-- window counts from the moment the question was asked. The wake pause stamps
-- it too, for one consistent "entered its pause" instant across both parked
-- states (wake expiry itself stays wake_at-driven; this column does not join
-- that sweep's predicate).
--
-- RUNTIME state, exactly like pending_question/pending_answer (049) and the
-- wake columns (059): read via taskColumns/scanTask, written ONLY by the
-- dedicated pause transitions, excluded from the task insert/upsert,
-- UpdateTaskTx, the TaskToCreate clone recipe, and the export record — a new
-- task never starts paused, and a definition write or recurrence spawn can
-- never carry or clobber it. Never cleared on resume: it is only meaningful
-- (and only read) while status is a paused state, and the next pause re-stamps
-- it.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS paused_at TIMESTAMPTZ;

-- Rows already paused when this migration lands have no recorded pause instant;
-- fall back to started_at (the sweep's previous proxy) so their expiry behavior
-- is unchanged rather than reset or — worse — "never expires" (the sweep
-- requires paused_at IS NOT NULL).
UPDATE tasks
SET paused_at = started_at
WHERE paused_at IS NULL
  AND started_at IS NOT NULL
  AND status IN ('paused_awaiting_input', 'paused_awaiting_wake');

-- No index: the expiry sweep filters status = 'paused_awaiting_input' first,
-- which the partial index idx_tasks_paused (049) already serves; the paused set
-- on a single-box deployment is tiny.
