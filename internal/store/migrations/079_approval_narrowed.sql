-- 079_approval_narrowed.sql — what a person approved when they narrowed a
-- card (agent_policy.critical_tool_narrowable_args, docs/APPROVAL-NARROWING.md,
-- ADR-0089).
--
-- Canonical JSON {"arg", "staged", "approved"}: the declared list argument,
-- every value the call was staged with (args_json keeps the whole staged call
-- unchanged) and the non-empty subset the person approved, in staged order.
-- Written in the same UPDATE as the claim that approves the card, so a card
-- is never approved narrowed without this record, and execution runs the
-- staged arguments with only that argument replaced. NULL is every other row
-- (a card approved whole, a declined or timed-out card, a tool that did not
-- opt in, and every row written before this column existed), which executes
-- exactly as before, so a nullable ADD with no backfill is the old behaviour.
ALTER TABLE approvals ADD COLUMN narrowed_json TEXT;

-- narrowable_arg freezes, at staging, the argument a person may narrow on a
-- pending card (the policy's critical_tool_narrowable_args entry then, when
-- the staged value is a narrowable list). The card shown, a reload of it and
-- the decision all use this value, never the boot policy at decision time, so
-- a restart with another entry can never narrow an argument the person did
-- not review. NULL = the card approves its call whole (every row written
-- before this column, and every card of a tool that did not opt in).
ALTER TABLE approvals ADD COLUMN narrowable_arg TEXT;
