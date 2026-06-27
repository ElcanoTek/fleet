-- Recovered-panic ledger (#241): safe.Recover persists every recovered panic
-- here (via cmd/fleet's registered PanicEventWriter) so operators can query what
-- crashed even when stdout/journald lost the line. Append-only; pruned by ops if
-- it ever grows.
CREATE TABLE panic_events (
  id       BIGSERIAL PRIMARY KEY,
  ts       BIGINT NOT NULL,        -- unix seconds
  location TEXT NOT NULL,          -- goroutine name / package (e.g. "runner.worker")
  message  TEXT NOT NULL,          -- the recovered panic value, stringified
  stack    TEXT NOT NULL           -- bounded debug.Stack() output
);

CREATE INDEX idx_panic_events_ts ON panic_events(ts DESC);
