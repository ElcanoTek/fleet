-- 057_add_recurrence_end.up.sql — recurrence end conditions.
--
-- A recurring task may declare when its recurrence stops: at an absolute
-- instant (recurrence_until — no occurrence is spawned with a fire time past
-- it) and/or after a total number of runs (recurrence_remaining — the count
-- of runs still allowed INCLUDING the row itself; each spawned occurrence
-- carries remaining-1, and the spawn is skipped when it would hit zero).
-- NULL = unbounded (the default, byte-identical behavior for every existing
-- task). Both are definition fields carried through the TaskToCreate clone
-- recipe, like allow_network.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS recurrence_until TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS recurrence_remaining INTEGER;
