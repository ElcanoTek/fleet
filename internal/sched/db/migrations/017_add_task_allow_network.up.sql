-- 017_add_task_allow_network.up.sql — per-task network egress toggle (#145).
--
-- Scheduled execution sandboxes default to network-sealed (--network=none) to
-- match the interactive lockdown path. allow_network is the explicit per-task
-- opt-in admitting outbound egress. NOT NULL DEFAULT FALSE backfills existing
-- rows to the sealed posture (the safe default).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS allow_network BOOLEAN NOT NULL DEFAULT FALSE;
