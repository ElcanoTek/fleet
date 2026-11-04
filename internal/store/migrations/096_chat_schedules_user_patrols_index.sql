-- The Patrols index (GET /api/patrols, store.ListUserPatrols) reads one
-- user's live recurring patrols on every rail mount. The existing indexes are
-- keyed for the scheduler (status, next_run_at) and per conversation, so that
-- per-user read filtered and sorted every user's rows. This partial index
-- covers exactly the rows the query can return (its own WHERE clause), keyed
-- by owner and in the query's sort order, and stays small: completed and
-- one-shot rows are not in it.
CREATE INDEX IF NOT EXISTS idx_chat_schedules_user_patrols
    ON chat_schedules (user_email, status, next_run_at, id)
    WHERE cron_expr <> '' AND status IN ('active', 'paused');
