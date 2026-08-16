-- 050_drop_turns_recovered_at.sql — drop the never-read recovery marker.
--
-- 041 added turns.recovered_at "to mark turns projected by startup recovery as
-- explicit interrupted turns" — but no reader ever landed: no scan, no API
-- field, no UI badge. The information it was meant to carry already reaches
-- every consumer through the recovery projection itself — the model-visible
-- interruption marker in canonical history and the synthetic `turn.error` SSE
-- frame ("server restarted mid-turn; partial work was recovered into history")
-- — so a queryable duplicate nobody queries is just write overhead and schema
-- noise. Its sibling from the same ALTER, history_committed_at, stays: that
-- one is load-bearing (the recovery scan and the input queue both read it).

-- migration-lint: allow-dangerous  drops the recovered_at column no code has ever read; recovery provenance lives in history + the synthetic turn.error event
ALTER TABLE turns DROP COLUMN IF EXISTS recovered_at;
