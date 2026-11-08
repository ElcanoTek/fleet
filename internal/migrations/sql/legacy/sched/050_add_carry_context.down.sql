-- 050_add_carry_context.down.sql — drop the recurring-task context-carry flag (#504).
ALTER TABLE tasks DROP COLUMN IF EXISTS carry_context;
