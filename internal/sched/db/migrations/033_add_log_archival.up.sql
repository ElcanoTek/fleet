-- Log archival (#272): compress (optionally encrypt) old task-run log payloads
-- in place to shrink the sched DB. session_data (JSONB) holds the live, plaintext
-- payload; once a row is archived the payload moves into session_data_gz (BYTEA,
-- gzip + optional AES-256-GCM) and session_data is nulled. session_compression
-- records the codec so GetLog can transparently inflate on read.
--
--   session_compression NULL/''  -> live plaintext JSON in session_data
--   session_compression 'gzip'   -> gzip payload in session_data_gz
--   'gzip+aes256gcm'             -> gzip then AES-256-GCM in session_data_gz
--
-- session_data is made nullable because an archived row carries its payload in
-- session_data_gz instead. Exactly one of the two columns is populated per row.
ALTER TABLE logs ALTER COLUMN session_data DROP NOT NULL;
ALTER TABLE logs ADD COLUMN IF NOT EXISTS session_data_gz BYTEA;
ALTER TABLE logs ADD COLUMN IF NOT EXISTS session_compression TEXT;
