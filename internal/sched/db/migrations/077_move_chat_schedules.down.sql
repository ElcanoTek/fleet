-- 077_move_chat_schedules.down.sql — move chat schedules back to the chat
-- schema.
--
-- Recreates chat.chat_schedules exactly as chat migrations 086 + 096 left it,
-- copies every schedule whose conversation still exists back (the chat
-- table's foreign key cascaded the others away) and empties the sched tables,
-- so a later up moves the then-current rows again. The run history has no
-- home in the old layout and is dropped. The conversation-delete trigger and
-- its function (created by the up) are dropped first: the old table's own
-- foreign key cascades again.
--
-- A fire whose turn was delivered but never settled (the process died between
-- the two, then the box was rolled back) is counted first, as the old engine
-- counted a fire in the same transaction as its turn: run_count advances,
-- last_run_at is the turn's time, a one-shot (or a patrol at max_runs) is
-- completed, and a patrol moves to the slot recorded with the delivery. The
-- old engine then does not fire that slot again. A turn is the fire's only by
-- provenance: patrol mode, the schedule's conversation, the fire's own key.
DO $$
BEGIN
    IF to_regclass('chat.conversations') IS NOT NULL THEN
        DROP TRIGGER IF EXISTS conversations_delete_chat_schedules ON chat.conversations;
    END IF;
    DROP FUNCTION IF EXISTS chat.conversations_delete_chat_schedules();
END
$$;

DO $$
BEGIN
    IF to_regclass('chat.conversations') IS NOT NULL THEN
        CREATE TABLE IF NOT EXISTS chat.chat_schedules (
            id               TEXT PRIMARY KEY,
            conversation_id  TEXT NOT NULL REFERENCES chat.conversations(id) ON DELETE CASCADE,
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
            ON chat.chat_schedules (status, next_run_at)
            WHERE status = 'active';
        CREATE INDEX IF NOT EXISTS idx_chat_schedules_conv
            ON chat.chat_schedules (conversation_id, status);
        CREATE INDEX IF NOT EXISTS idx_chat_schedules_user_patrols
            ON chat.chat_schedules (user_email, status, next_run_at, id)
            WHERE cron_expr <> '' AND status IN ('active', 'paused');

        IF to_regclass('chat.chat_input_queue') IS NOT NULL THEN
            UPDATE chat_schedules s
               SET run_count = s.fire_run_number,
                   last_run_at = q.created_at,
                   status = CASE
                       WHEN s.status IN ('active', 'paused')
                        AND (s.cron_expr = '' OR (s.max_runs > 0 AND s.fire_run_number >= s.max_runs))
                       THEN 'completed' ELSE s.status END,
                   -- The slot recorded with the delivery, unless the cadence
                   -- was edited since (def_version moved): then the edit's
                   -- own recomputed next_run_at stands.
                   next_run_at = CASE
                       WHEN s.cron_expr <> '' AND s.fire_next_run_at IS NOT NULL
                        AND s.fire_def_version IS NOT DISTINCT FROM s.def_version
                       THEN s.fire_next_run_at
                       ELSE s.next_run_at END
              FROM chat.chat_input_queue q
             WHERE s.fire_id IS NOT NULL
               AND s.fire_run_number = s.run_count + 1
               AND q.conversation_id = s.conversation_id
               AND q.mode = 'patrol'
               AND q.client_input_id = 'chat-patrol:' || s.id || ':' || s.fire_run_number || ':'
                   || CASE s.fire_trigger WHEN 'manual' THEN 'm' ELSE 's' END || '-' || s.fire_id;
        END IF;

        INSERT INTO chat.chat_schedules (
            id, conversation_id, user_email, title, prompt, cron_expr, timezone,
            duration_seconds, status, next_run_at, last_run_at, run_count,
            max_runs, created_at, updated_at
        )
        SELECT s.id, s.conversation_id, s.user_email, s.title, s.prompt, s.cron_expr, s.timezone,
               s.duration_seconds, s.status, s.next_run_at, s.last_run_at, s.run_count,
               s.max_runs, s.created_at, s.updated_at
          FROM chat_schedules s
          JOIN chat.conversations c ON c.id = s.conversation_id
        ON CONFLICT (id) DO NOTHING;

        DELETE FROM chat_schedule_runs;
        DELETE FROM chat_schedules;
    END IF;
END
$$;
