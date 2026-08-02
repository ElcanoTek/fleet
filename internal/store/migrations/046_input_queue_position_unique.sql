-- Position is the queue's FIFO key. Older writers allocated it with an
-- unlocked MAX/MIN read, so concurrent submissions could tie. Normalize any
-- existing non-terminal ties deterministically before making uniqueness
-- structural. Running/injected rows are included because recovery may move
-- either back to queued; leaving a tie there would make recovery hit the new
-- constraint.
WITH ranked AS (
  SELECT id,
         ROW_NUMBER() OVER (
           PARTITION BY conversation_id
           ORDER BY position, created_at, id
         ) AS position
    FROM chat_input_queue
   WHERE state IN ('queued', 'running', 'injected')
)
UPDATE chat_input_queue q
   SET position = ranked.position
  FROM ranked
 WHERE q.id = ranked.id;

DROP INDEX chat_input_queue_pending;

CREATE UNIQUE INDEX chat_input_queue_active_position
  ON chat_input_queue (conversation_id, position)
  WHERE state IN ('queued', 'running', 'injected');

-- The terminal-retention sweep filters only by transition time. Keep that
-- cleanup bounded without indexing live queue rows that it must never delete.
CREATE INDEX chat_input_queue_terminal_retention
  ON chat_input_queue (updated_at)
  WHERE state IN ('completed', 'cancelled');
