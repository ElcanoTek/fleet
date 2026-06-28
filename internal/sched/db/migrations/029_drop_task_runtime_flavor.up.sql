-- 029_drop_task_runtime_flavor.up.sql — remove the per-task runtime-flavor
-- override. The runtime-"flavor" system (native-inprocess / native-acp / external
-- acp) has been removed along with the ACP layer: fleet runs exactly one native
-- in-process agent loop, so a per-task runtime override no longer means anything.
-- The historical 018_add_task_runtime_flavor migration is left intact (the ledger
-- is append-only); this forward migration drops the now-unused column.
ALTER TABLE tasks DROP COLUMN IF EXISTS runtime_flavor;
