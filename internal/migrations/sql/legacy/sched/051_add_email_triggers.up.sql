-- 051_add_email_triggers.up.sql — event-driven triggers 2.0: email ingress (#511).
--
-- Extends the existing webhook-trigger machinery (#177, migration 021) with an
-- EMAIL trigger kind: an inbound email — delivered by an email provider's
-- inbound-parse webhook as a normalized JSON payload — spawns a governed run,
-- exactly like POST /triggers/{slug} but with email-specific security controls
-- (approved senders, DKIM/SPF policy, attachment limits, Message-ID dedup). The
-- template task stays trigger_type='webhook' (inert; cron never runs it); the
-- email-ness lives on task_triggers.kind, so NO new TaskTrigger table and NO new
-- task TriggerType — the one proven spawn path is reused, not forked.

-- kind discriminates a trigger row: 'webhook' (the #177 default) or 'email'.
-- Existing rows default to 'webhook', preserving current behavior exactly.
ALTER TABLE task_triggers ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'webhook';

-- email_policy holds the email-kind security controls as JSONB (nullable; NULL
-- for webhook rows) — approved_senders, require_dkim/spf, attachment caps. JSONB
-- (like tasks.sandbox_limits / loop_config) keeps the column count flat and lets
-- the policy shape evolve without a migration per knob.
ALTER TABLE task_triggers ADD COLUMN IF NOT EXISTS email_policy JSONB;

-- allow_event_triggers is the per-task opt-in the issue's SECURITY DEFAULT
-- requires: an event-spawned run inherits the template's write-capable MCP
-- connectors ONLY when the task explicitly opted in. Default FALSE ⇒ an
-- event-spawned run gets native tools only (no connectors), so an untrusted
-- inbound event can never auto-escalate. Threaded through db.go like
-- allow_network. (The #177 webhook path is unchanged — it always inherits.)
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS allow_event_triggers BOOLEAN NOT NULL DEFAULT FALSE;

-- trigger_events records every ACCEPTED inbound event: it is both the
-- idempotency ledger (UNIQUE(trigger_id, idempotency_key) — a duplicate delivery
-- is a no-op, satisfying the dedup criterion) and the audit trail that ties the
-- spawned run back to its originating event (run_id) and captures the reply
-- target (sender + message_id + subject) for reply-back.
CREATE TABLE IF NOT EXISTS trigger_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    trigger_id      UUID NOT NULL REFERENCES task_triggers(id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    sender          TEXT NOT NULL DEFAULT '',
    subject         TEXT NOT NULL DEFAULT '',
    message_id      TEXT NOT NULL DEFAULT '',
    run_id          UUID,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (trigger_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_trigger_events_trigger_id ON trigger_events(trigger_id);
-- Reverse lookup event↔run for reply-back (find the inbound event a run answers).
CREATE INDEX IF NOT EXISTS idx_trigger_events_run_id ON trigger_events(run_id) WHERE run_id IS NOT NULL;
