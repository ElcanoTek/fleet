-- 063_retire_analyzing_status.up.sql — retire the leftover moc 'analyzing' status (#1077).
--
-- Fleet never writes 'analyzing': error analysis is a post-terminal annotation,
-- not a lifecycle transition, and workers cannot report the status
-- (IsValidReportedStatus rejects it). The constant survived #124 only so
-- leftover moc-imported rows still decoded — which meant every claim / lease /
-- recovery / reporting query had to special-case a status the current loop
-- never produces. Rewrite those leftover rows to 'running', the in-flight
-- status the loop actually understands: recovery re-queues them on lease
-- expiry exactly as it would have with 'analyzing'.
UPDATE tasks SET status = 'running' WHERE status = 'analyzing';

-- Rebuild the serialization-key partial index (055) without the retired
-- status; no row can match it after the rewrite above.
DROP INDEX IF EXISTS idx_tasks_serialization_active;
CREATE INDEX IF NOT EXISTS idx_tasks_serialization_active
    ON tasks (serialization_key)
    WHERE serialization_key IS NOT NULL
    AND status IN ('leased', 'running');
