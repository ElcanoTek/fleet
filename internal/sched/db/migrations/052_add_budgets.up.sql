-- 052_add_budgets.up.sql — per-principal rolling budgets (#601 part 2).
--
-- One small table holds every budget: {scope: user|key|project, principal id,
-- rolling window: day|week|month, soft/hard bounds in dollars AND tokens}.
-- Spend against a budget is NEVER accumulated here — enforcement recomputes the
-- principal's current-window spend from the metering the governed core already
-- persists (task_iterations ⋈ tasks, plus the chat turn_metrics log), i.e. the
-- #601 part-1 usage read model. The only mutable state a budget carries is the
-- soft-alert crossing marker, persisted so a soft alert fires exactly once per
-- window crossing and a process restart cannot re-alert.
--
-- The column is time_window (not "window") because WINDOW is a reserved word.

CREATE TABLE IF NOT EXISTS budgets (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope        TEXT NOT NULL CHECK (scope IN ('user', 'key', 'project')),
    principal_id TEXT NOT NULL,
    time_window  TEXT NOT NULL CHECK (time_window IN ('day', 'week', 'month')),
    -- Soft/hard bounds. All nullable: NULL = that bound is not configured. A
    -- CHECK requires at least one bound so an inert row cannot exist.
    soft_usd     DOUBLE PRECISION CHECK (soft_usd    IS NULL OR soft_usd    >= 0),
    hard_usd     DOUBLE PRECISION CHECK (hard_usd    IS NULL OR hard_usd    >= 0),
    soft_tokens  BIGINT           CHECK (soft_tokens IS NULL OR soft_tokens >= 0),
    hard_tokens  BIGINT           CHECK (hard_tokens IS NULL OR hard_tokens >= 0),
    CHECK (soft_usd IS NOT NULL OR hard_usd IS NOT NULL OR soft_tokens IS NOT NULL OR hard_tokens IS NOT NULL),
    -- Soft-alert dedup marker: the UTC start of the window the one alert was
    -- fired for. The claim UPDATE is conditional on this differing from the
    -- current window start, so concurrent creates race to exactly one alert
    -- and a restart (state is in the DB, not memory) cannot re-alert.
    soft_alert_window_start TIMESTAMPTZ,
    soft_alert_at           TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One budget per (scope, principal, window); POST /admin/budgets upserts on
    -- this key.
    UNIQUE (scope, principal_id, time_window)
);
