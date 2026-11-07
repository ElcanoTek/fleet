-- 076_chat_schedules.up.sql — the sched engine's chat schedule tables
-- (#2049 M5, ADR-0104).
--
-- In-chat patrols (recurring cron) and timers (one-shot) used to live in
-- chat.chat_schedules (chat migrations 086 + 096) and were fired by a second
-- tick loop in the chat server. They become rows of the scheduler: this
-- migration creates their tables in the sched schema; 077 moves the rows.
--
-- chat_schedules keeps the chat table's columns, ids and values verbatim
-- (owner, cadence, timezone, status, next_run_at, run_count, ...), so the API
-- serves the same objects and a schedule keeps its idempotency keys: the next
-- run is run_count + 1, exactly as it would have been. lease_owner and
-- lease_expires_at are the engine's lease (the tasks table's model): a fire
-- claims the row, hands the turn to the chat server, then settles; a claim
-- that dies mid-fire expires and is re-claimed, and the chat side's
-- idempotency key makes the re-delivery a no-op.
--
-- fire_id / fire_trigger / fire_run_number are the fire a lease is for. They
-- survive the lease's expiry: the recovery pass re-claims the SAME fire (same
-- id, so the same idempotency key), whatever the schedule's status or due
-- time — a Run now on a paused schedule included — and only a settle (or a
-- pause/cancel revoking the fire) clears them. A delivery that fails keeps
-- its fire and lease, the lease pushed out by a backoff that grows with
-- fire_failures, so a failing schedule waits its turn instead of being
-- claimed again on every pass.
--
-- conversation_id is NOT a foreign key any more: the conversation is in
-- another schema (and store). A fire whose conversation is gone cancels the
-- schedule, as the chat claim always did for a soft-deleted chat.
CREATE TABLE IF NOT EXISTS chat_schedules (
    id               TEXT PRIMARY KEY,
    conversation_id  TEXT NOT NULL,
    user_email       TEXT NOT NULL,
    title            TEXT NOT NULL DEFAULT '',
    prompt           TEXT NOT NULL,
    cron_expr        TEXT NOT NULL DEFAULT '',
    timezone         TEXT NOT NULL DEFAULT 'UTC',
    duration_seconds INTEGER NOT NULL DEFAULT 0,
    status           TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'paused', 'completed', 'cancelled')),
    next_run_at      BIGINT NOT NULL,
    last_run_at      BIGINT,
    run_count        INTEGER NOT NULL DEFAULT 0,
    max_runs         INTEGER NOT NULL DEFAULT 0,
    created_at       BIGINT NOT NULL,
    updated_at       BIGINT NOT NULL,
    lease_owner      TEXT,
    lease_expires_at TIMESTAMPTZ,
    fire_id          TEXT,
    fire_trigger     TEXT CHECK (fire_trigger IN ('schedule', 'manual')),
    fire_run_number  INTEGER,
    fire_failures    INTEGER NOT NULL DEFAULT 0
);

-- Leases whose holder may have died, for the recovery pass.
CREATE INDEX IF NOT EXISTS idx_chat_schedules_leased
    ON chat_schedules (lease_expires_at)
    WHERE lease_owner IS NOT NULL;

-- The engine's due scan (status, next_run_at), the per-conversation list,
-- and the Patrols index (same predicate as chat migration 096).
CREATE INDEX IF NOT EXISTS idx_chat_schedules_due
    ON chat_schedules (status, next_run_at)
    WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_chat_schedules_conv
    ON chat_schedules (conversation_id, status);
CREATE INDEX IF NOT EXISTS idx_chat_schedules_user_patrols
    ON chat_schedules (user_email, status, next_run_at, id)
    WHERE cron_expr <> '' AND status IN ('active', 'paused');

-- chat_schedule_runs is the run history: one row per settled fire. trigger_kind is
-- 'schedule' (the tick) or 'manual' (Run now). outcome: 'queued' (a turn was
-- enqueued; input_id names it), 'coalesced' (Run now found a patrol turn
-- already queued or running; input_id names that one), 'busy' (the chat had a
-- patrol in flight, an approval waiting or a full queue: the fire was skipped
-- to the next slot) or 'gone' (the conversation was deleted or the owner lost
-- access: the schedule was cancelled). Pruned by the scheduler's run-history
-- retention sweep with the tasks' history (FLEET_RUN_LOG_RETENTION_DAYS).
CREATE TABLE IF NOT EXISTS chat_schedule_runs (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    schedule_id TEXT NOT NULL REFERENCES chat_schedules(id) ON DELETE CASCADE,
    run_number  INTEGER NOT NULL,
    trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('schedule', 'manual')),
    outcome     TEXT NOT NULL CHECK (outcome IN ('queued', 'coalesced', 'busy', 'gone')),
    input_id    TEXT NOT NULL DEFAULT '',
    due_at      BIGINT,
    fired_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_chat_schedule_runs_schedule
    ON chat_schedule_runs (schedule_id, fired_at DESC);
-- One history row per enqueued turn: makes the backfill below re-runnable.
CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_schedule_runs_input
    ON chat_schedule_runs (schedule_id, input_id)
    WHERE outcome = 'queued' AND input_id <> '';
