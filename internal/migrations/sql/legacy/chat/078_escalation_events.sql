-- 078_escalation_events.sql — one row per chat turn on which fleet saw the
-- user struggling (docs/CHAT-ESCALATION.md): the decision-model classifier
-- scored the user's message as pushback on the assistant, or a free
-- end-of-turn signal fired (a tool-error streak, a loop-guard block, a forced
-- compaction, a very long turn). The admin Users page counts these per user.
--
-- Recorded whether or not a "switch to the advanced model" card was shown
-- (observe mode, a suppressed card): the count is how often users struggled,
-- not how often fleet said so. No message text is stored, only the signal
-- name and, for the classifier, its probability.
--
-- Conventions match the chat schema: BIGINT unix-seconds timestamps and
-- ON DELETE CASCADE with the conversation, like turn_metrics and
-- tool_call_log.

CREATE TABLE escalation_events (
  id              BIGSERIAL PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  turn_id         TEXT NOT NULL,
  user_email      TEXT NOT NULL,
  created_at      BIGINT NOT NULL,
  -- classifier | signal
  source          TEXT NOT NULL,
  -- frustration (classifier) | tool_errors | loop_guard | compaction | long_turn
  signal          TEXT NOT NULL,
  -- The classifier's P(pushback); NULL for a signal row.
  probability     DOUBLE PRECISION,
  -- The model the turn ran on.
  model           TEXT NOT NULL DEFAULT '',
  -- TRUE when this event staged a suggestion card.
  suggested       BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX idx_escalation_events_user ON escalation_events(user_email, created_at DESC);
CREATE INDEX idx_escalation_events_conv ON escalation_events(conversation_id, created_at DESC);
