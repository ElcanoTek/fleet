-- 069_add_lineage_id.up.sql — job lineage for per-job scheduled workspaces (#1543).
--
-- Every non-worktree scheduled run used to work in the shared workspace root,
-- so one job's leftovers (scripts, downloads, status files, other clients'
-- reports) fed another job's reasoning and its end-of-run audit. Each run now
-- gets <workspace-root>/tasks/<lineage_id>/: one directory per JOB, shared by
-- every occurrence, re-run and clone of that job and by nothing else, so a
-- daily job still finds its own previous downloads and nobody else's.
--
-- lineage_id is the key every run of one job shares. A task created fresh is
-- its own lineage (lineage_id = id); the TaskToCreate clone recipe carries it
-- to recurrence occurrences, re-runs and clones. Definition-stable (carried by
-- the upsert and UpdateTaskTx, unlike the immutable spawn pointers), never
-- exported (a re-imported definition starts its own lineage on the target).
-- TEXT like source_task_id / previous_occurrence_id, the other lineage keys.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS lineage_id TEXT;
-- Every pre-existing row is its own lineage; the next occurrence carries it on.
UPDATE tasks SET lineage_id = id::text WHERE lineage_id IS NULL;
