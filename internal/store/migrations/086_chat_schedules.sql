-- 086_chat_schedules.sql — in-chat patrols and timers.
--
-- A chat_schedules row attaches a recurring cron patrol or a one-shot timer
-- directly to a conversation. When next_run_at arrives, the chat server claims
-- the due schedule and enqueues a server-initiated turn (InputKindChatPatrol)
-- into chat_input_queue for that conversation, preserving its full transcript,
-- workspace files, persistent Python state, connector selection, and
-- conversation auto-approve setting.
CREATE TABLE IF NOT EXISTS chat_schedules (
    id               TEXT PRIMARY KEY,
    conversation_id  TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
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
    updated_at       BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_chat_schedules_due
    ON chat_schedules (status, next_run_at)
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_chat_schedules_conv
    ON chat_schedules (conversation_id, status);
