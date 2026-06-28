-- Per-task persona override (#221): an optional personas/<name>.yaml whose
-- domain-expertise block is injected into the scheduled task's system prompt,
-- so different task types can use specialized personas. NULL/empty = the
-- runner's global persona (assistant.yaml by default).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS persona TEXT;
