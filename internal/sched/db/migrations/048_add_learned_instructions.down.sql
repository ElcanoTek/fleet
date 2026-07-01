-- 048_add_learned_instructions.down.sql — drop the self-improving-memory
-- feedback + learned-instruction tables (#516).
DROP TABLE IF EXISTS task_learned_instructions;
DROP TABLE IF EXISTS task_feedback;
