-- 060_add_task_title.up.sql — the operator-facing task title.
--
-- A short human label shown wherever a task is listed (Recent Tasks, the
-- upcoming timeline, the log modal, the SLA report). It exists because the
-- Operations Center previously identified a task by the first ~80 characters of
-- its prompt, so operators were putting a title line at the head of the prompt
-- to tell their jobs apart — a workaround that puts display text into the model
-- input.
--
-- title is DELIBERATELY NOT the existing `name` column (036):
--   - name is the import/export IDENTITY key and carries a partial UNIQUE index,
--     so two jobs may never share one. Titles are labels, not identities:
--     "Daily deal health scan" may legitimately appear on more than one task.
--   - storage.scheduleNextRecurrence CLEARS name on every occurrence it spawns
--     (a carried name would collide with the row it was cloned from), so a name
--     survives exactly one run of a recurring task — useless as a display label.
-- title has neither constraint: it is non-unique, and TaskToCreate carries it,
-- so every occurrence, re-run and clone of a job keeps the same title.
--
-- '' (the default) means untitled, and every existing task reads back untitled —
-- the UI falls back to the prompt's first line exactly as it does today.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS title TEXT NOT NULL DEFAULT '';

-- The Recent Tasks search box matches title alongside prompt and id; the ILIKE
-- is a leading-wildcard match, so this is a plain btree on the lowercased value
-- to keep the common "starts with" prefix search off a sequential scan.
CREATE INDEX IF NOT EXISTS idx_tasks_title ON tasks (LOWER(title)) WHERE title <> '';
