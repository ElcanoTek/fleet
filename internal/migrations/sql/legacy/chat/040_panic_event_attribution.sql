-- Universal AgentTool panic containment (#795): enrich the append-only panic
-- ledger with the same opaque incident/run/tool attribution emitted to logs and
-- Sentry. Empty strings preserve legacy safe.Recover rows that have no run or
-- tool identity. Raw panic values, stacks, tool input, and output are not stored;
-- new rows place only the value-free class in the legacy message column.
ALTER TABLE panic_events
  ADD COLUMN incident_id     TEXT NOT NULL DEFAULT '',
  ADD COLUMN boundary        TEXT NOT NULL DEFAULT '',
  ADD COLUMN tool_name       TEXT NOT NULL DEFAULT '',
  ADD COLUMN tool_call_id    TEXT NOT NULL DEFAULT '',
  ADD COLUMN run_mode        TEXT NOT NULL DEFAULT '',
  ADD COLUMN task_id         TEXT NOT NULL DEFAULT '',
  ADD COLUMN conversation_id TEXT NOT NULL DEFAULT '';

-- Rows written by pre-040 binaries may contain the formatted recovered value
-- and a stack. Remove those diagnostics during the privacy-boundary migration;
-- their timestamp/location remain available for aggregate incident history.
UPDATE panic_events
SET message = 'legacy', stack = ''
WHERE incident_id = '';

CREATE INDEX idx_panic_events_incident_id
  ON panic_events(incident_id)
  WHERE incident_id <> '';
