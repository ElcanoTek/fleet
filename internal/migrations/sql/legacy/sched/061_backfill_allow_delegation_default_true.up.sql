-- 061_backfill_allow_delegation_default_true.up.sql — sub-agents default-on (#1043).
--
-- Product decision (issue #1043, amending ADR-0007): sub-agent delegation is ON
-- by default — the spawn_subagent tool is registered and the PARENT AGENT
-- decides whether to spawn; the operator only ever opts OUT (per task via
-- allow_delegation=false, or fleet-wide via FLEET_SUBAGENTS_ENABLED=false /
-- Admin → Features). The enablement compose inverts from OR (either opts in)
-- to AND (both default true; either can kill).
--
-- Flip the column default for rows inserted outside the app, and backfill
-- EXISTING rows to true — the issue explicitly says not to grandfather old
-- rows off (behavior change; recorded in the CHANGELOG). Pre-#1043, false was
-- simply the default nobody had to choose, so it does not encode an explicit
-- operator opt-out.
ALTER TABLE tasks ALTER COLUMN allow_delegation SET DEFAULT TRUE;
UPDATE tasks SET allow_delegation = TRUE WHERE allow_delegation = FALSE;
