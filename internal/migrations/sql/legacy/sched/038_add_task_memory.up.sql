-- 038_add_task_memory.up.sql — persistent task memory for agent
-- self-improvement (#285, closing #198 + #322).
--
-- task_memories backs the per-task "Captain's Log" opt-in (the
-- instruction_self_improve flag, finally given runtime effect): persistent,
-- task-scoped facts a scheduled run can carry across executions. Auto-committed
-- by the agent via the remember/recall tools; keyed by (task_id, key). This is
-- runtime STATE in the scheduler DB, NOT the operator-owned client-config
-- bundle — so writing it freely is reproducibility-safe (the bundle stays a
-- versioned, operator-authored artifact; nothing here mutates it, and fleet
-- never writes the bundle or git).
--
-- Self-improvement of prompts/knowledge is handled by the EXISTING propose_note
-- flow (agent_notes, migration 015), which is also DB-backed and admin-reviewed.
-- Agent-authored client-bundle skills are intentionally out of scope: skills
-- stay operator-authored so the bundle remains a reproducible git artifact.

-- ON DELETE CASCADE: a task's memories are its own state and die with it.
CREATE TABLE IF NOT EXISTS task_memories (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id     UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    key         TEXT NOT NULL,                   -- snake_case identifier, <= 128 chars
    value       TEXT NOT NULL,                   -- free-form (JSON / markdown / text)
    created_at  BIGINT NOT NULL,                 -- unix seconds (matches notes/memories)
    updated_at  BIGINT NOT NULL,
    UNIQUE (task_id, key)
);

-- Backs ListTaskMemories (task_id) and the LRU eviction order (oldest updated_at
-- evicted first when a task exceeds FLEET_TASK_MEMORY_MAX_KEYS).
CREATE INDEX IF NOT EXISTS idx_task_memories_task_updated
    ON task_memories (task_id, updated_at);
