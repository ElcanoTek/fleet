-- 0203_chat_schedule_run_reason: why a scheduled chat fire was busy.
--
-- A 'busy' row in sched.chat_schedule_runs is a slot the scheduler skipped,
-- for one of three reasons the delivery tells apart: an earlier patrol turn
-- still queued or running ('patrol_in_flight'), an approval card waiting on
-- the owner ('approval_pending') or a full input queue ('queue_full'). The
-- chat UI reads 'approval_pending' rows to tell the owner their patrol is
-- being skipped until they answer the card. '' is every other outcome, and a
-- busy row recorded before this migration (its reason is unknown).
ALTER TABLE sched.chat_schedule_runs
    ADD COLUMN IF NOT EXISTS reason TEXT NOT NULL DEFAULT ''
        CONSTRAINT chat_schedule_runs_reason_check
        CHECK (reason IN ('', 'patrol_in_flight', 'approval_pending', 'queue_full'));
