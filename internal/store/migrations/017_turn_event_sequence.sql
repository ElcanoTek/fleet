-- 017_turn_event_sequence.sql — cursor-based pagination for turn events (#189).
--
-- The original turn_events ledger keys rows by (turn_id, event_id), where
-- event_id is a per-TURN monotonic counter restarted at 1 for each turn. That
-- is enough for the single-turn /stream reattach path (LoadTurnEvents) but
-- gives the read path no stable, cross-turn ordering to paginate a whole
-- conversation by. Loading a long conversation therefore meant materialising
-- every event at once.
--
-- This migration adds a per-CONVERSATION monotonic `sequence` (the opaque
-- pagination cursor) plus a denormalised `conversation_id` and `turn_index`
-- (the ordinal of the turn within its conversation) so a page query is a
-- single-table, index-backed range scan — no join back to `turns` per read.
--
-- The change is purely ADDITIVE: existing columns and the (turn_id, event_id)
-- primary key are untouched, so the current LoadTurnEvents reattach path keeps
-- working unchanged. New rows get their sequence assigned by the store at
-- insert time (see InsertTurnEvents); this migration backfills existing rows.

-- turns.turn_index: 0-based ordinal of a turn within its conversation, ordered
-- by started_at then turn_id (a stable tiebreaker). Backfilled for existing
-- rows; new turns get it assigned in CreateTurn.
ALTER TABLE turns ADD COLUMN turn_index INT NOT NULL DEFAULT 0;

UPDATE turns t
SET turn_index = sub.rn - 1
FROM (
  SELECT turn_id,
         ROW_NUMBER() OVER (
           PARTITION BY conversation_id
           ORDER BY started_at, turn_id
         ) AS rn
  FROM turns
) sub
WHERE t.turn_id = sub.turn_id;

-- turn_events gains a denormalised conversation_id (copied from the owning
-- turn), the per-conversation `sequence` cursor, and a copy of turn_index so
-- the page read needs no join. conversation_id has the same ON DELETE CASCADE
-- semantics as the existing turn_id FK, so sweeps/deletes still cascade.
ALTER TABLE turn_events
  ADD COLUMN conversation_id TEXT REFERENCES conversations(id) ON DELETE CASCADE,
  ADD COLUMN sequence BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN turn_index INT NOT NULL DEFAULT 0;

-- Backfill conversation_id + turn_index from the owning turn.
UPDATE turn_events te
SET conversation_id = t.conversation_id,
    turn_index      = t.turn_index
FROM turns t
WHERE te.turn_id = t.turn_id;

-- Backfill the per-conversation sequence. ROW_NUMBER over (created_at, turn_id,
-- event_id) gives a stable, gap-free ordering within each conversation that
-- matches the order the events were produced.
UPDATE turn_events te
SET sequence = sub.rn
FROM (
  SELECT turn_id, event_id,
         ROW_NUMBER() OVER (
           PARTITION BY conversation_id
           ORDER BY created_at, turn_id, event_id
         ) AS rn
  FROM turn_events
) sub
WHERE te.turn_id = sub.turn_id AND te.event_id = sub.event_id;

-- Now that every row has a real conversation_id, make it NOT NULL so the store
-- can rely on it. (Added nullable above so the backfill UPDATE could run.)
ALTER TABLE turn_events ALTER COLUMN conversation_id SET NOT NULL;

-- The pagination index: range-scan a conversation's events by sequence, and
-- enforce that the per-conversation sequence is unique (the store assigns it
-- as MAX(sequence)+row, so a duplicate would signal a concurrency bug).
CREATE UNIQUE INDEX turn_events_conv_seq ON turn_events (conversation_id, sequence);
