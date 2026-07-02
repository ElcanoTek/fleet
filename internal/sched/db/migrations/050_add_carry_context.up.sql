-- 050_add_carry_context.up.sql — recurring-task context carry (#504).
--
-- When true on a RECURRING task, each run injects a BOUNDED handoff from the
-- prior run (its final answer, clamped) as a "## Previous Run" prompt section —
-- NOT a whole-transcript replay (deterministic + cheap, per the triage). Default
-- false = start fresh each run (unchanged behavior). Threaded through db.go like
-- allow_network. (049 is reserved by the in-flight ask/notify pause work; this
-- is 050 to stay collision-free.)
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS carry_context BOOLEAN NOT NULL DEFAULT FALSE;
