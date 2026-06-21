-- 005_memories.sql — user-scoped long-term memories.
--
-- Uses IF NOT EXISTS because this branch temporarily shipped memories as
-- 004_memories before dev added 004_lockdown. Local/dev databases that ran
-- that branch already have this table but do not have schema_migrations=5.

CREATE TABLE IF NOT EXISTS memories (
  id         TEXT PRIMARY KEY,
  user_email TEXT NOT NULL,
  content    TEXT NOT NULL,
  source     TEXT NOT NULL DEFAULT 'manual', -- manual|chat
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_memories_user_updated ON memories(user_email, updated_at DESC);
