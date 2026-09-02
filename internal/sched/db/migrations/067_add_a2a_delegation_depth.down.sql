-- 067_add_a2a_delegation_depth.down.sql — drop the inbound A2A delegation depth (#1368).
ALTER TABLE tasks DROP COLUMN IF EXISTS a2a_delegation_depth;
