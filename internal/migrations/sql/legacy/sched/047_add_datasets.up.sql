-- 047_add_datasets.up.sql — dataset / table agent (#514).
--
-- The "1000-row agent" workflow: a typed table where an agent works each row
-- in the background toward a per-dataset goal, writing STRUCTURED results
-- back as PROPOSED cell values a human reviews (bulk approve) before they
-- land. Rows run through the ONE governed loop (sandbox + ceilings +
-- redaction); free-form answers become a row note, never a cell mutation.
--
-- datasets.columns is a marshaled []models.DatasetColumn ({name, type,
-- output, description}); output columns define the structured-output schema
-- the write-back must conform to. dataset_rows.cells holds the INPUT values
-- (untrusted data — delimited as such in the per-row prompt, never
-- interpolated into instructions); proposed holds the validated agent
-- write-back awaiting review.
CREATE TABLE IF NOT EXISTS datasets (
    id          UUID PRIMARY KEY,
    name        TEXT NOT NULL,
    goal        TEXT NOT NULL,
    columns     JSONB NOT NULL,
    model       TEXT NOT NULL DEFAULT '',
    persona     TEXT NOT NULL DEFAULT '',
    -- idle | running | paused. Running is an in-process claim by THIS
    -- server's dataset runner; boot resets stale 'running' to 'paused'.
    status      TEXT NOT NULL DEFAULT 'idle',
    concurrency INTEGER NOT NULL DEFAULT 2,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS dataset_rows (
    id          UUID PRIMARY KEY,
    dataset_id  UUID NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
    row_index   INTEGER NOT NULL,
    cells       JSONB NOT NULL,
    -- pending | running | proposed | approved | failed
    status      TEXT NOT NULL DEFAULT 'pending',
    proposed    JSONB,
    result_note TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    attempts    INTEGER NOT NULL DEFAULT 0,
    cost_usd    DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (dataset_id, row_index)
);

-- Backs the runner's pending-claim scan and the status-filtered row listing.
CREATE INDEX IF NOT EXISTS idx_dataset_rows_dataset_status ON dataset_rows (dataset_id, status, row_index);
