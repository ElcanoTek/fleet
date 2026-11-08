-- 018_add_task_runtime_flavor.up.sql — per-task runtime-flavor override (#158).
--
-- The Operations Center agent picker mirrors chat's per-conversation runtime
-- selection: a task may name a runtime flavor (native-inprocess / native-acp /
-- an external acp flavor). NULL = use the bundle's global scheduled runtime. An
-- external flavor still routes through the fail-closed scheduled-external gate
-- (allow_ungoverned_scheduled_agents) at dispatch.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS runtime_flavor TEXT;
