-- 001_initial.sql — the Postgres schema as of the first shipped release.
--
-- Future migrations live in 002_*.sql, 003_*.sql, etc. Each file runs
-- inside its own transaction and bumps schema_migrations on success
-- (see migrations.go applyMigrations). Never edit a migration that's
-- already shipped — add a new one instead.

CREATE TABLE conversations (
  id         TEXT PRIMARY KEY,
  user_email TEXT NOT NULL,
  title      TEXT NOT NULL,
  persona    TEXT NOT NULL DEFAULT 'victoria',
  -- model: per-chat OpenRouter slug override. Empty = use the server-
  -- configured primary. Set via PUT /conversations/{id}/model.
  model      TEXT NOT NULL DEFAULT '',
  pinned     BOOLEAN NOT NULL DEFAULT FALSE,
  created_at BIGINT NOT NULL,          -- unix seconds
  updated_at BIGINT NOT NULL
);

CREATE INDEX idx_conv_user_updated ON conversations(user_email, updated_at DESC);
CREATE INDEX idx_conv_user_pinned  ON conversations(user_email, pinned, updated_at DESC);

CREATE TABLE messages (
  id              BIGSERIAL PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  role            TEXT NOT NULL,        -- user|assistant|tool
  type            TEXT NOT NULL,        -- text|reasoning|tool_call|tool_result|turn_summary
  content         TEXT NOT NULL,        -- JSON blob (shape lives in agent.HistoryEntry)
  created_at      BIGINT NOT NULL
);

CREATE INDEX idx_msg_conv ON messages(conversation_id, id);

CREATE TABLE approvals (
  id              TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  user_email      TEXT NOT NULL,
  tool_name       TEXT NOT NULL,
  args_json       TEXT NOT NULL,
  status          TEXT NOT NULL DEFAULT 'pending', -- pending|approved|rejected
  result_text     TEXT,
  created_at      BIGINT NOT NULL,
  resolved_at     BIGINT
);

CREATE INDEX idx_approvals_conv ON approvals(conversation_id, status);

CREATE TABLE turn_metrics (
  id                BIGSERIAL PRIMARY KEY,
  conversation_id   TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  user_email        TEXT NOT NULL,
  completed_at      BIGINT NOT NULL,
  cost_usd          DOUBLE PRECISION NOT NULL DEFAULT 0,
  prompt_tokens     BIGINT NOT NULL DEFAULT 0,
  completion_tokens BIGINT NOT NULL DEFAULT 0,
  cached_tokens     BIGINT NOT NULL DEFAULT 0,
  cancelled         BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX idx_turn_metrics_user ON turn_metrics(user_email, completed_at DESC);
CREATE INDEX idx_turn_metrics_conv ON turn_metrics(conversation_id);

-- users.email is case-insensitive at the application layer
-- (normalizeEmail in store/users.go lowercases + trims). Keeping the DB
-- plain TEXT avoids a dependency on the CITEXT extension.
CREATE TABLE users (
  email         TEXT PRIMARY KEY,
  password_hash TEXT NOT NULL,
  created_at    BIGINT NOT NULL,
  updated_at    BIGINT NOT NULL
);
