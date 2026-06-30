-- Reverse 041_add_task_allow_delegation.
ALTER TABLE tasks DROP COLUMN IF EXISTS allow_delegation;
