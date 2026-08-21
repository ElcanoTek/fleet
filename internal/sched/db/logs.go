package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Log operations

// runLogHistoryKeep bounds how many superseded transcripts run_logs retains
// per task (#history): the trim runs inside the same transaction as the
// copy-on-overwrite, so the cap can never be exceeded between sweeps.
const runLogHistoryKeep = 20

// archiveSupersededLog copies the task's CURRENT logs row (if any) into
// run_logs, then trims that task's history past runLogHistoryKeep. Called
// inside the AddLog/AddLogRaw transaction immediately before the upsert that
// would otherwise destroy the row — so history costs nothing for a task that
// only ever writes one transcript (retry-free, never resumed). The columns
// travel verbatim: an archived (gz+codec) payload stays archived.
func archiveSupersededLog(ctx context.Context, tx *sql.Tx, taskID uuid.UUID) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO run_logs (task_id, session_data, session_data_gz, session_compression)
		SELECT task_id, session_data, session_data_gz, session_compression
		FROM logs WHERE task_id = $1`, taskID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		DELETE FROM run_logs WHERE task_id = $1 AND id NOT IN (
			SELECT id FROM run_logs WHERE task_id = $1
			ORDER BY superseded_at DESC, id DESC LIMIT $2
		)`, taskID, runLogHistoryKeep)
	return err
}

// upsertLog is the shared write path of AddLog/AddLogRaw: archive the row the
// upsert would clobber (per-attempt history), then write the new payload live
// (plaintext JSON in session_data); the archival columns are reset so a
// re-write of a previously archived row returns it to the live, uncompressed
// state.
func (db *Database) upsertLog(ctx context.Context, taskID uuid.UUID, sessionJSON []byte) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := archiveSupersededLog(ctx, tx, taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO logs (task_id, session_data, session_data_gz, session_compression)
		VALUES ($1, $2, NULL, NULL)
		ON CONFLICT (task_id) DO UPDATE SET
			session_data = EXCLUDED.session_data,
			session_data_gz = NULL,
			session_compression = NULL`,
		taskID, string(sessionJSON)); err != nil {
		return err
	}
	return tx.Commit()
}

// AddLog stores a log session for a task, archiving any transcript it
// supersedes into run_logs first (per-attempt history).
func (db *Database) AddLog(ctx context.Context, taskID uuid.UUID, session *models.LogSession) error {
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return db.upsertLog(ctx, taskID, sessionJSON)
}

// AddLogRaw stores a pre-serialized log session verbatim (legacy import,
// docs/LEGACY-IMPORT.md). The payload travels byte-for-byte from the source
// system's logs.session_data — no unmarshal/remarshal round-trip that could
// drop fields a newer/older LogSession shape doesn't know about. Same
// archive-then-upsert semantics as AddLog.
func (db *Database) AddLogRaw(ctx context.Context, taskID uuid.UUID, sessionJSON []byte) error {
	return db.upsertLog(ctx, taskID, sessionJSON)
}

// LogExists reports whether a run-log row exists for the task. Used by the
// legacy importer's skip-by-default re-run posture (#713), mirroring TaskExists.
func (db *Database) LogExists(ctx context.Context, taskID uuid.UUID) (bool, error) {
	var exists bool
	err := db.conn.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM logs WHERE task_id = $1)", taskID).Scan(&exists)
	return exists, err
}

// decodeLogRow turns one logs row into JSON bytes, transparently inflating (and
// decrypting, when a key is configured) an archived payload (#272). Exactly one
// of sessionData / gz is populated: a live row carries plaintext in sessionData
// with an empty codec; an archived row carries bytes in gz with a non-empty
// codec and a NULL sessionData.
func (db *Database) decodeLogRow(sessionData *string, gz []byte, codec string) ([]byte, error) {
	if codec != "" {
		return decodeArchive(gz, db.archiveKey, codec)
	}
	if sessionData != nil {
		return []byte(*sessionData), nil
	}
	return nil, errors.New("log row has neither live nor archived payload")
}

// GetLog gets the log session for a task, transparently inflating an archived
// payload so callers see no difference between live and archived logs (#272).
func (db *Database) GetLog(ctx context.Context, taskID uuid.UUID) (*models.LogSession, error) {
	var sessionData *string
	var gz []byte
	var codec sql.NullString
	err := db.conn.QueryRowContext(ctx,
		"SELECT session_data, session_data_gz, session_compression FROM logs WHERE task_id = $1",
		taskID).Scan(&sessionData, &gz, &codec)
	if err != nil {
		return nil, err
	}
	raw, err := db.decodeLogRow(sessionData, gz, codec.String)
	if err != nil {
		return nil, err
	}
	var session models.LogSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// ListRunLogHistory lists a task's superseded transcripts (per-attempt
// history), newest first: id + when each was superseded. The payloads are
// fetched one at a time via GetRunLogEntry — a history listing must never
// drag every archived transcript across the wire.
func (db *Database) ListRunLogHistory(ctx context.Context, taskID uuid.UUID) ([]models.RunLogMeta, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, superseded_at FROM run_logs
		WHERE task_id = $1 ORDER BY superseded_at DESC, id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	metas := []models.RunLogMeta{}
	for rows.Next() {
		var m models.RunLogMeta
		if err := rows.Scan(&m.ID, &m.SupersededAt); err != nil {
			return nil, err
		}
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

// GetRunLogEntry fetches one superseded transcript by history id, scoped to
// the task so a caller authorized for one task can never read another task's
// history by guessing ids. Transparently inflates archived payloads, exactly
// like GetLog.
func (db *Database) GetRunLogEntry(ctx context.Context, taskID uuid.UUID, entryID int64) (*models.LogSession, error) {
	var sessionData *string
	var gz []byte
	var codec sql.NullString
	err := db.conn.QueryRowContext(ctx, `
		SELECT session_data, session_data_gz, session_compression
		FROM run_logs WHERE task_id = $1 AND id = $2`,
		taskID, entryID).Scan(&sessionData, &gz, &codec)
	if err != nil {
		return nil, err
	}
	raw, err := db.decodeLogRow(sessionData, gz, codec.String)
	if err != nil {
		return nil, err
	}
	var session models.LogSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// logScanChunk is the keyset page size for ForEachLog / GetAllLogs.
// One page of decoded sessions is the peak payload memory (#1122).
const logScanChunk = 64

// GetAllLogs gets all stored log sessions, transparently inflating archived
// payloads (#272). Implemented via ForEachLog so the scan itself is
// keyset-paginated; the returned map still holds every session (test /
// runner callers have small tables). Admin pipeline-metrics uses
// ForEachLog directly so it never materializes the full set (#1122).
func (db *Database) GetAllLogs(ctx context.Context) (map[uuid.UUID]*models.LogSession, error) {
	logs := make(map[uuid.UUID]*models.LogSession)
	err := db.ForEachLog(ctx, func(taskID uuid.UUID, session *models.LogSession) error {
		logs[taskID] = session
		return nil
	})
	return logs, err
}

// ForEachLog visits every stored log session in task_id order, inflating
// archived payloads. Sessions are fetched in keyset pages of logScanChunk
// so peak memory is one page + the callback's own working set (#1122).
// A decode/unmarshal failure skips that row (same as GetAllLogs).
func (db *Database) ForEachLog(ctx context.Context, fn func(uuid.UUID, *models.LogSession) error) error {
	var after uuid.UUID
	haveAfter := false
	for {
		n, last, err := db.scanLogPage(ctx, haveAfter, after, fn)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		after = last
		haveAfter = true
		if n < logScanChunk {
			return nil
		}
	}
}

func (db *Database) scanLogPage(ctx context.Context, haveAfter bool, after uuid.UUID, fn func(uuid.UUID, *models.LogSession) error) (int, uuid.UUID, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if haveAfter {
		rows, err = db.conn.QueryContext(ctx, `
			SELECT task_id, session_data, session_data_gz, session_compression
			FROM logs
			WHERE task_id > $1
			ORDER BY task_id
			LIMIT $2`, after, logScanChunk)
	} else {
		rows, err = db.conn.QueryContext(ctx, `
			SELECT task_id, session_data, session_data_gz, session_compression
			FROM logs
			ORDER BY task_id
			LIMIT $1`, logScanChunk)
	}
	if err != nil {
		return 0, uuid.Nil, err
	}
	defer rows.Close()

	n := 0
	var last uuid.UUID
	for rows.Next() {
		var taskID uuid.UUID
		var sessionData *string
		var gz []byte
		var codec sql.NullString
		if err := rows.Scan(&taskID, &sessionData, &gz, &codec); err != nil {
			return n, last, err
		}
		last = taskID
		n++
		raw, err := db.decodeLogRow(sessionData, gz, codec.String)
		if err != nil {
			continue
		}
		var session models.LogSession
		if err := json.Unmarshal(raw, &session); err != nil {
			continue
		}
		if err := fn(taskID, &session); err != nil {
			return n, last, err
		}
	}
	return n, last, rows.Err()
}
