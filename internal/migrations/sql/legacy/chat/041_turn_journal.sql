-- Durable interactive turn journal + canonical projection (#798). The SSE
-- turn_events ledger stays a delivery/view layer (its tool.result payloads are
-- 4 KB UI previews); this journal is the full-fidelity side-effect record: one
-- row per model-visible tool-call INTENT (written before dispatch) and one per
-- GOVERNED tool result (the exact model-visible bytes from the #793 boundary,
-- written before the next provider step). Startup recovery projects a stranded
-- journal into canonical messages history so the next model turn sees work
-- that already happened instead of repeating it.
CREATE TABLE turn_journal (
  turn_id     TEXT    NOT NULL REFERENCES turns(turn_id) ON DELETE CASCADE,
  seq         BIGINT  NOT NULL,
  kind        TEXT    NOT NULL CHECK (kind IN ('tool_intent','tool_result')),
  call_id     TEXT    NOT NULL,
  tool_name   TEXT    NOT NULL,
  content     TEXT    NOT NULL,
  is_err      BOOLEAN NOT NULL DEFAULT FALSE,
  synthesized BOOLEAN NOT NULL DEFAULT FALSE,
  created_at  BIGINT  NOT NULL,
  PRIMARY KEY (turn_id, seq)
);

-- Idempotency: at most one intent and one result per tool call per turn.
-- Recovery pairs calls with results by call_id; a duplicate write is a loud
-- constraint violation, never a silent second side-effect record.
CREATE UNIQUE INDEX turn_journal_call ON turn_journal (turn_id, kind, call_id);

-- Projection provenance. Nullable: every existing messages INSERT uses an
-- explicit column list, and branch copies / import / post-turn approval
-- resolutions deliberately keep NULL provenance so they never collide with
-- the projection's uniqueness guarantee below.
ALTER TABLE messages
  ADD COLUMN turn_id  TEXT,
  ADD COLUMN turn_seq BIGINT;

-- Belt-and-braces against double projection (repeated startup recovery, crash
-- mid-recovery): one canonical row per (turn, position).
CREATE UNIQUE INDEX messages_turn_seq
  ON messages (turn_id, turn_seq)
  WHERE turn_id IS NOT NULL AND turn_seq IS NOT NULL;

-- Terminal-commit + recovery markers. history_committed_at is NULL until the
-- canonical projection transaction commits — turn.completed is not
-- authoritative before that. recovered_at marks turns projected by startup
-- recovery as explicit interrupted turns.
ALTER TABLE turns
  ADD COLUMN history_committed_at BIGINT,
  ADD COLUMN recovered_at         BIGINT;

CREATE INDEX idx_turns_uncommitted ON turns (status) WHERE history_committed_at IS NULL;
