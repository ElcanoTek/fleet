-- 058_add_run_log_history.up.sql — per-attempt run log history (superseded
-- transcripts).
--
-- logs keeps exactly one row per task (the LATEST transcript); every re-run of
-- the same task id — a retry, an ask-pause resume (#510), a self-wake cycle —
-- upserts over it, destroying the prior attempt's transcript. run_logs is the
-- copy-on-overwrite archive: immediately before an upsert would clobber an
-- existing logs row, that row is copied here verbatim (live or archived
-- payload alike, so the archival codec columns travel unchanged). A task that
-- only ever runs once writes nothing here — history costs storage only when
-- an overwrite would otherwise have destroyed a transcript.
--
-- superseded_at records when the copy was taken (= when the newer transcript
-- landed).
CREATE TABLE IF NOT EXISTS run_logs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id UUID NOT NULL,
    superseded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    session_data JSONB,
    session_data_gz BYTEA,
    session_compression TEXT
);

CREATE INDEX IF NOT EXISTS idx_run_logs_task_superseded
    ON run_logs(task_id, superseded_at DESC);
