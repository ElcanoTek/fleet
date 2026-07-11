-- Usage is an accounting record, not conversation content. The original
-- cascade erased cost/token history whenever a conversation was hard-deleted
-- by a user or retention sweep, making admin totals appear to reset.
--
-- Keep conversation_id as provenance when its conversation still exists. The
-- usage read model already LEFT JOINs conversations and deliberately degrades
-- deleted-conversation model/project dimensions into the empty bucket.
ALTER TABLE turn_metrics
  DROP CONSTRAINT IF EXISTS turn_metrics_conversation_id_fkey;
