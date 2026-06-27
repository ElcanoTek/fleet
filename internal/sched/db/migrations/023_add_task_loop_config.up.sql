-- Iterative verification loops (#179): an optional worker+verifier loop config
-- on a task, plus a per-iteration telemetry table.
--
-- loop_config is NULL for an ordinary one-shot task (the prior behaviour, the
-- vast majority of tasks); a non-null value turns the task into a bounded
-- worker→verify→retry loop. Stored as JSONB; the runner decodes it.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS loop_config JSONB;

-- Per-iteration telemetry for a looped task. One row per worker+verify cycle,
-- so operators can see how many iterations a task took, what each verification
-- returned, and the per-iteration cost. Cascade-deleted with the task.
CREATE TABLE IF NOT EXISTS task_iterations (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id               UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    iteration_number      INTEGER NOT NULL,
    started_at            TIMESTAMPTZ NOT NULL,
    completed_at          TIMESTAMPTZ,
    worker_session_id     TEXT,
    exit_condition_result TEXT,
    cost_usd              NUMERIC(12,6),
    prompt_tokens         BIGINT,
    completion_tokens     BIGINT,
    status                TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_task_iterations_task_id ON task_iterations (task_id, iteration_number);
