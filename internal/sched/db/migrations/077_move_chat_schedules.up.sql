-- 077_move_chat_schedules.up.sql — move chat schedules from the chat schema
-- into the sched engine's tables (#2049 M5, ADR-0104).
--
-- The chat store always migrates before the scheduler at boot, so
-- on every install that has run a chat build chat.chat_schedules exists here
-- (empty on a fresh install). A database without it (a scheduler-only test
-- database, or a re-run after the move) skips the block.
--
-- The ACCESS EXCLUSIVE lock is taken before the copy: a chat process still
-- firing the old table (the outgoing binary mid-update) either commits first,
-- and its advanced next_run_at / run_count is what gets copied, or waits and
-- then fails on the dropped table. Either way every schedule fires exactly
-- once per slot across the upgrade: never twice (the copy and the drop are
-- one transaction, so the old table cannot fire after the copy), never
-- skipped (next_run_at is copied as it stands, so a slot that fell due during
-- the restart fires on the new engine's first tick, as it would have on the
-- old one).
--
-- History: the chat table kept only run_count and last_run_at, but every fire
-- left a chat_input_queue row keyed chat-patrol:<id>:<run>[:<unix> for Run
-- now]. Those still in the queue's retention window are copied as the start
-- of each schedule's run history.
DO $$
BEGIN
    IF to_regclass('chat.chat_schedules') IS NOT NULL THEN
        LOCK TABLE chat.chat_schedules IN ACCESS EXCLUSIVE MODE;

        INSERT INTO chat_schedules (
            id, conversation_id, user_email, title, prompt, cron_expr, timezone,
            duration_seconds, status, next_run_at, last_run_at, run_count,
            max_runs, created_at, updated_at
        )
        SELECT id, conversation_id, user_email, title, prompt, cron_expr, timezone,
               duration_seconds, status, next_run_at, last_run_at, run_count,
               max_runs, created_at, updated_at
          FROM chat.chat_schedules
        ON CONFLICT (id) DO NOTHING;

        IF to_regclass('chat.chat_input_queue') IS NOT NULL THEN
            INSERT INTO chat_schedule_runs (schedule_id, run_number, trigger_kind, outcome, input_id, fired_at)
            SELECT cs.id,
                   split_part(q.client_input_id, ':', 3)::integer,
                   CASE WHEN split_part(q.client_input_id, ':', 4) = '' THEN 'schedule' ELSE 'manual' END,
                   'queued',
                   q.id,
                   to_timestamp(q.created_at)
              FROM chat.chat_input_queue q
              JOIN chat_schedules cs ON cs.id = split_part(q.client_input_id, ':', 2)
             WHERE q.client_input_id LIKE 'chat-patrol:%'
               AND split_part(q.client_input_id, ':', 3) ~ '^[0-9]{1,9}$'
            ON CONFLICT DO NOTHING;
        END IF;

        DROP TABLE chat.chat_schedules;
    END IF;
END
$$;
