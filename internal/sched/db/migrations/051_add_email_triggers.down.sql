-- 051_add_email_triggers.down.sql — drop email-ingress triggers (#511).
DROP TABLE IF EXISTS trigger_events;
ALTER TABLE tasks DROP COLUMN IF EXISTS allow_event_triggers;
ALTER TABLE task_triggers DROP COLUMN IF EXISTS email_policy;
ALTER TABLE task_triggers DROP COLUMN IF EXISTS kind;
