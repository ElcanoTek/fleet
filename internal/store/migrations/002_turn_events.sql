-- 002_turn_events.sql — per-turn SSE event log for resume-after-crash.
--
-- The in-memory turnBuffer handles the hot path (phone-lock, refresh
-- within retention TTL). These tables are crash insurance: if
-- chat-server dies mid-turn, we can still tell a reattaching client
-- "that turn is gone, here's what we had" instead of hanging the UI
-- forever. Also lets retention outlive the process.

CREATE TABLE turns (
  turn_id         TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  started_at      BIGINT NOT NULL,
  -- finished_at NULL + status 'running' = mid-flight. On server startup
  -- any such row gets upgraded to 'error' with a synthetic terminal
  -- event so a reconnecting client sees a clean EOF.
  finished_at     BIGINT,
  status          TEXT NOT NULL DEFAULT 'running'
                  CHECK (status IN ('running', 'completed', 'cancelled', 'error'))
);

CREATE INDEX idx_turns_conv ON turns(conversation_id, started_at DESC);
CREATE INDEX idx_turns_running ON turns(status) WHERE status = 'running';

CREATE TABLE turn_events (
  turn_id    TEXT NOT NULL REFERENCES turns(turn_id) ON DELETE CASCADE,
  event_id   INTEGER NOT NULL,
  event_name TEXT NOT NULL,
  data_json  TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  PRIMARY KEY (turn_id, event_id)
);
