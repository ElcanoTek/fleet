-- 006_ensure_lockdown.sql — compatibility shim for branch-local DBs.
--
-- Some development databases applied an earlier branch version where schema
-- version 004 was memories. After merging dev, 004 is lockdown, so those DBs
-- skip 004_lockdown as already-applied. Keep this idempotent migration so all
-- databases converge on the expected conversations.lockdown column.

ALTER TABLE conversations
    ADD COLUMN IF NOT EXISTS lockdown BOOLEAN NOT NULL DEFAULT FALSE;
