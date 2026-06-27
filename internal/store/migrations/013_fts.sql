-- Full-text search across conversations + message content (#308).
--
-- Postgres-native FTS (tsvector + GIN), no external search engine. Two surfaces:
--   1. conversation TITLES — a generated tsvector column kept in sync by Postgres.
--   2. message CONTENT — messages.content is a JSON blob, so we extract the
--      plaintext into a dedicated table (message_search_content) whose tsvector is
--      what we index. Extraction happens at write time (AppendHistory) and, for
--      pre-existing rows, via a startup backfill (internal/store/fts_backfill.go)
--      run OUTSIDE this migration so it never holds a long table lock.

-- Conversation title FTS — generated column is always in sync, no triggers.
ALTER TABLE conversations
  ADD COLUMN search_vector tsvector
  GENERATED ALWAYS AS (to_tsvector('english', coalesce(title, ''))) STORED;

CREATE INDEX conversations_fts_idx ON conversations USING gin(search_vector);

-- Extracted message plaintext + its generated tsvector. One row per searchable
-- message (user/assistant text + user summaries). message_id is unique so the
-- backfill is idempotent (ON CONFLICT DO NOTHING) and ON DELETE CASCADE keeps it
-- in lockstep with messages.
CREATE TABLE message_search_content (
  id              BIGSERIAL PRIMARY KEY,
  conversation_id TEXT   NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  message_id      BIGINT NOT NULL UNIQUE REFERENCES messages(id) ON DELETE CASCADE,
  content         TEXT   NOT NULL,
  search_vector   tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
  created_at      BIGINT NOT NULL
);

CREATE INDEX message_search_content_fts_idx ON message_search_content USING gin(search_vector);
CREATE INDEX message_search_content_conv_idx ON message_search_content(conversation_id);
