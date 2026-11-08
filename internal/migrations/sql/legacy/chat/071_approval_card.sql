-- 071_approval_card.sql — the readable approval card a bundle-declared
-- describer produced when the call was staged
-- (agent_policy.critical_tool_card_describers, docs/APPROVAL-CARD-DESCRIBERS.md).
--
-- Canonical JSON, validated by the server before it is written and again on
-- every read. NULL means no card: no describer is declared for the tool, the
-- describer failed, or the row predates this column. The client then renders
-- the generic arguments card, so a nullable ADD with no backfill is exactly
-- the old behaviour for every existing row.
ALTER TABLE approvals ADD COLUMN card_json TEXT;
