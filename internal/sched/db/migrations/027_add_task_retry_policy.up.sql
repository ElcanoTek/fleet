-- Configurable retry policy (#201): optional per-task backoff + failure-class
-- gating for whole-task retries. NULL = the legacy hard-coded policy (transient
-- failures only, 30s→10m exponential backoff). A non-null value overrides the
-- backoff knobs and broadens/narrows which failure classes retry. The retry
-- COUNT remains Task.max_retries (single source of truth). Stored as JSONB; the
-- runner decodes it.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS retry_policy JSONB;
