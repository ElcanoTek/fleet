-- 020_approval_timeouts.sql — configurable approval timeouts (#225).
--
-- expires_at : unix-seconds deadline (BIGINT, matching created_at/resolved_at)
--              after which a still-pending approval is auto-denied by the
--              server-side expiry sweep — the default-DENY-on-timeout contract
--              for the web approval path. NULL = legacy row or "no expiry"; the
--              sweep and the UI countdown both treat NULL/0 as "never expires".
-- approval_timeout_seconds : per-conversation override (INTEGER) of the global
--              FLEET_APPROVAL_TIMEOUT_SECONDS default. NULL = use the global
--              default, so existing rows are unaffected.
ALTER TABLE approvals ADD COLUMN expires_at BIGINT;

-- Drives the expiry sweep's hot query (pending rows past their deadline)
-- without scanning resolved approvals.
CREATE INDEX idx_approvals_pending_expires ON approvals (expires_at) WHERE status = 'pending';

ALTER TABLE conversations ADD COLUMN approval_timeout_seconds INTEGER;
