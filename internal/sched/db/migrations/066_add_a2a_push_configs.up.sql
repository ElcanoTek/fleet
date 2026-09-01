-- A2A per-task push-notification configs (#1279 Phase 2).
--
-- One row per (task, config id): an external A2A caller registers a webhook
-- URL to be notified when the task's caller-visible state changes. The id is
-- CLIENT-SUPPLIED when given (the spec lets a caller manage multiple configs
-- per task by its own ids; the official TCK round-trips its own id), server-
-- minted otherwise — hence the composite primary key rather than a surrogate.
--
-- token_sealed / auth_credentials_sealed hold caller-supplied secrets sealed
-- with internal/secretbox (AAD-bound to task_id + id); the plaintext never
-- lands in the database or logs. last_pushed_status is the delivery marker
-- the push dispatcher compares against tasks.status — the task ROW is the
-- event source (same as the A2A streams), so no transition bus is needed.
--
-- ON DELETE CASCADE: configs live exactly as long as their task row; the
-- spec's "persist until task completion or explicit deletion" is satisfied by
-- keeping them past terminal states and letting task deletion reap them.
CREATE TABLE IF NOT EXISTS a2a_push_configs (
    task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    id TEXT NOT NULL,
    url TEXT NOT NULL,
    token_sealed BYTEA,
    auth_scheme TEXT NOT NULL DEFAULT '',
    auth_credentials_sealed BYTEA,
    last_pushed_status TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, id)
);

-- The dispatcher's work scan joins on task_id; the PK already serves it, but
-- an explicit index keeps the plan stable if the PK ever changes shape.
CREATE INDEX IF NOT EXISTS idx_a2a_push_configs_task ON a2a_push_configs (task_id);
