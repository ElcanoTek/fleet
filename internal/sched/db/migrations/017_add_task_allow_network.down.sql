-- Reverse 017_add_task_allow_network.
ALTER TABLE tasks DROP COLUMN IF EXISTS allow_network;
