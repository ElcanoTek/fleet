-- 048_add_learned_instructions.up.sql — self-improving memory: feedback →
-- versioned, revertible learned instructions (#516, layer on #285 Captain's Log).
--
-- Two tables. task_feedback captures raw signals on a task's runs (thumbs +
-- optional critique). task_learned_instructions holds DISTILLED, VERSIONED
-- per-task instructions injected at run time — enterprise default is
-- staged: a distilled instruction is 'proposed' and must be ACTIVATED by a
-- human before it changes behavior; activation supersedes the prior active
-- version (revert = re-activate an older one). Every instruction links the
-- evidence signal_count that produced it. Nothing here writes the operator's
-- client bundle or agent-authored skills — those stay out of scope.
CREATE TABLE IF NOT EXISTS task_feedback (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id    UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    -- 'up' | 'down'. A 'down' (optionally with critique) is the signal that
    -- drives distillation; 'up' is recorded for before/after context.
    rating     TEXT NOT NULL,
    critique   TEXT NOT NULL DEFAULT '',
    -- consumed flips true once a signal has been folded into a distilled
    -- instruction, so the next distillation only sees fresh feedback.
    consumed   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at BIGINT NOT NULL,
    created_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_task_feedback_task ON task_feedback (task_id, consumed, created_at);

CREATE TABLE IF NOT EXISTS task_learned_instructions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id      UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    version      INTEGER NOT NULL,           -- monotonic per task
    content      TEXT NOT NULL,
    -- 'proposed' (distilled, awaiting activation) | 'active' (injected at run
    -- time; at most one per task) | 'archived' (superseded / reverted).
    status       TEXT NOT NULL DEFAULT 'proposed',
    signal_count INTEGER NOT NULL DEFAULT 0, -- evidence signals that produced it
    created_at   BIGINT NOT NULL,
    activated_at BIGINT,
    activated_by TEXT NOT NULL DEFAULT '',
    UNIQUE (task_id, version)
);
-- At most one active instruction per task (the run-time injection target).
CREATE UNIQUE INDEX IF NOT EXISTS idx_task_learned_active
    ON task_learned_instructions (task_id) WHERE status = 'active';
