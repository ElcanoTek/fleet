-- 041_add_task_allow_delegation.up.sql — per-task agent-delegation opt-in (#264).
--
-- allow_delegation is the explicit per-task opt-in that registers the
-- spawn_subagent (delegation) native tool for a scheduled run, letting it fan out
-- scoped subtasks to governed child runs. It composes with the fleet-wide
-- FLEET_SUBAGENTS_ENABLED operator flag as OR (either enables it). NOT NULL
-- DEFAULT FALSE backfills existing rows to "no delegation" — the safe default, so
-- behaviour is byte-for-byte unchanged for every existing task.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS allow_delegation BOOLEAN NOT NULL DEFAULT FALSE;
