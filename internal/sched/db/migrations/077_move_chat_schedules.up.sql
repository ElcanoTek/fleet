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
                   -- Old keys: <run> (scheduled) and <run>:<unix> (Run now). Keys
                   -- since the fold carry the kind: <run>:s-<fire> / <run>:m-<fire>.
                   CASE WHEN split_part(q.client_input_id, ':', 4) = ''
                          OR split_part(q.client_input_id, ':', 4) LIKE 's-%'
                        THEN 'schedule' ELSE 'manual' END,
                   'queued',
                   q.id,
                   to_timestamp(q.created_at)
              FROM chat.chat_input_queue q
              JOIN chat_schedules cs ON cs.id = split_part(q.client_input_id, ':', 2)
                                    AND cs.conversation_id = q.conversation_id
             WHERE q.client_input_id LIKE 'chat-patrol:%'
               -- Provenance: only server-owned patrol turns, in the schedule's
               -- own conversation. client_input_id is caller-supplied; a user
               -- input that borrows a chat-patrol: key is not a fire.
               AND q.mode = 'patrol'
               AND split_part(q.client_input_id, ':', 3) ~ '^[0-9]{1,9}$'
            ON CONFLICT DO NOTHING;
        END IF;

        DROP TABLE chat.chat_schedules;
    END IF;
END
$$;

-- fire_next_run_at: the slot a delivered patrol fire moves the schedule to,
-- written with the delivery (the settle recomputes it). Only 077's down reads
-- it, to count a delivered-but-unsettled fire as the old engine would have.
ALTER TABLE chat_schedules ADD COLUMN IF NOT EXISTS fire_next_run_at BIGINT;

-- A conversation's schedules go with it. conversation_id cannot be a foreign
-- key across the schemas, so without the chat table's old ON DELETE CASCADE
-- every path that hard-deletes conversations (single and bulk delete, the
-- TTL sweep, the unpinned-cap eviction, the soft-delete purge, account
-- erasure, webhook and attachment cleanup, and any added later) would leave
-- the schedules and their run history — the user's prompt text — behind. One
-- statement-level trigger on chat.conversations restores the cascade for all
-- of them (run history follows by this schema's FK).
--
-- It lives here, not in a chat migration, so this migration's down removes it
-- and a rollback never moves the chat schema's version (the previous release
-- refuses to boot on a chat schema newer than it knows). The function lives in
-- the chat schema beside its trigger and is a no-op if the sched table is
-- gone. Skipped on a database without chat.conversations — one the scheduler
-- initialised first (a sched-only CLI verb before any `fleet serve`); there
-- `fleet serve` installs the same trigger once both schemas exist, retrying
-- in the background until it succeeds (db.KeepChatScheduleCascade), and
-- removes schedules orphaned meanwhile.
DO $$
BEGIN
    IF to_regclass('chat.conversations') IS NOT NULL THEN
        CREATE OR REPLACE FUNCTION chat.conversations_delete_chat_schedules() RETURNS trigger
        LANGUAGE plpgsql AS $fn$
        BEGIN
            IF to_regclass('sched.chat_schedules') IS NOT NULL THEN
                EXECUTE 'DELETE FROM sched.chat_schedules WHERE conversation_id IN (SELECT id FROM deleted_conversations)';
            END IF;
            RETURN NULL;
        END
        $fn$;

        DROP TRIGGER IF EXISTS conversations_delete_chat_schedules ON chat.conversations;
        CREATE TRIGGER conversations_delete_chat_schedules
            AFTER DELETE ON chat.conversations
            REFERENCING OLD TABLE AS deleted_conversations
            FOR EACH STATEMENT
            EXECUTE FUNCTION chat.conversations_delete_chat_schedules();
    END IF;
END
$$;
