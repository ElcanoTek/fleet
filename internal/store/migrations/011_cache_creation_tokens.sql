-- Persist the cache-write token count alongside the cache-read count so net
-- prompt-cache savings can be computed from the DB alone. Existing rows
-- backfill to 0, which matches the pre-migration behaviour (cache-creation was
-- simply not recorded).
ALTER TABLE turn_metrics
  ADD COLUMN cache_creation_tokens BIGINT NOT NULL DEFAULT 0;
