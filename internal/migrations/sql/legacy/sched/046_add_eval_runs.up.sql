-- 046_add_eval_runs.up.sql — eval & regression harness results (#502).
--
-- eval_runs stores one row per `fleet eval run <set>` invocation: the set-level
-- aggregate (total/passed/mean_score against the set's threshold gate) plus the
-- full per-case results as JSONB (a marshaled []evals.CaseResult — scorer
-- verdicts, judge reasoning, cost/token/duration per case). Eval DEFINITIONS
-- (the golden prompts + rubrics) deliberately do NOT live here — they are
-- client content in the external bundle's evals/ dir (ADR-0006); this table is
-- the operational record that makes regressions comparable across runs.
--
-- bundle_sha is the replayed bundle's content fingerprint
-- (evals.BundleFingerprint): two rows are an apples-to-apples model comparison
-- only when their bundle_sha matches, and a bundle-edit regression shows up as
-- a pass-rate drop across differing shas.
CREATE TABLE IF NOT EXISTS eval_runs (
    id           UUID PRIMARY KEY,
    eval_set     TEXT NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL,
    bundle_sha   TEXT NOT NULL DEFAULT '',
    total        INTEGER NOT NULL,
    passed       INTEGER NOT NULL,
    mean_score   DOUBLE PRECISION NOT NULL,
    threshold    DOUBLE PRECISION NOT NULL,
    pass         BOOLEAN NOT NULL,
    cost_usd     DOUBLE PRECISION NOT NULL DEFAULT 0,
    results      JSONB NOT NULL
);

-- Backs the newest-first per-set history listing (fleet eval history / the
-- latest-run baseline lookup).
CREATE INDEX IF NOT EXISTS idx_eval_runs_set_started ON eval_runs (eval_set, started_at DESC);
