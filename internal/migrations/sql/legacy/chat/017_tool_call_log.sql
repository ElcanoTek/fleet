-- 017_tool_call_log.sql — persistent, queryable audit log of every agent tool
-- call in an interactive chat turn (#224).
--
-- Today the only durable record of what tools ran is the raw SSE event stream in
-- turn_events; reconstructing "which tools ran in this conversation, and did they
-- error?" means replaying that stream. This table is a first-class, indexed
-- ledger: one row per tool call, paired from the turn's tool_call + tool_result
-- history entries after the turn completes.
--
-- Conventions match the rest of the chat schema: BIGINT unix-seconds timestamps
-- (not TIMESTAMPTZ), and ON DELETE CASCADE so deleting a conversation cleans up
-- its audit rows automatically (same as messages / turn_metrics / turn_events).
--
-- args_summary / result_summary are REDACTED text — secrets in tool input/output
-- are scrubbed by the shared internal/redact pass before insertion, consistent
-- with the host-side-credentials invariant. Raw secret values never land here.

CREATE TABLE tool_call_log (
  id              BIGSERIAL PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  turn_id         TEXT NOT NULL,
  user_email      TEXT NOT NULL,
  tool_name       TEXT NOT NULL,
  -- Redacted, length-capped summary of the model-emitted tool input (the raw
  -- JSON args) and the tool's textual result. Never raw secret material.
  args_summary    TEXT NOT NULL DEFAULT '',
  result_summary  TEXT NOT NULL DEFAULT '',
  -- Outcome: TRUE when the tool returned an error result (or never produced a
  -- result — an interrupted/cancelled call).
  is_error        BOOLEAN NOT NULL DEFAULT FALSE,
  started_at      BIGINT NOT NULL,           -- unix seconds, turn start
  -- duration_ms is per-call wall time when derivable; NULL when the result
  -- timing could not be paired (e.g. a tool_call with no matching tool_result).
  duration_ms     BIGINT
);

-- The per-conversation audit endpoint reads (conversation_id, started_at DESC);
-- the supplementary indexes anticipate the cross-cutting queries the issue calls
-- out (by user, by tool) without committing to those endpoints in this change.
CREATE INDEX idx_tool_call_log_conv ON tool_call_log(conversation_id, started_at DESC);
CREATE INDEX idx_tool_call_log_user ON tool_call_log(user_email, started_at DESC);
CREATE INDEX idx_tool_call_log_tool ON tool_call_log(tool_name, started_at DESC);
