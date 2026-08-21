package db

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// archiveScanChunk is the keyset page size for ArchiveOldLogs. Each
// candidate holds a live (uncompressed) payload, so the page is small
// on purpose — first-run archival of a year-old table stays bounded (#1122).
const archiveScanChunk = 32

// cleanupEligibleSubquery selects terminal task ids eligible for pruning (#252):
// older than the cutoff ($2) but NOT among the most recent $1 runs of their
// (prompt, recurrence) bucket — so the last-known state of any task is always
// kept regardless of age. Non-terminal tasks and rows with a NULL completed_at
// are never selected. Reused for the logs + tasks deletes within one tx; safe to
// run twice because the tasks ranking is unchanged between them (only logs are
// deleted first).
const cleanupEligibleSubquery = `
	SELECT id FROM (
		SELECT id, completed_at,
		       ROW_NUMBER() OVER (
		           PARTITION BY prompt, recurrence
		           ORDER BY completed_at DESC NULLS LAST
		       ) AS rn
		FROM tasks
		WHERE status IN ('success', 'error', 'cancelled')
	) ranked
	WHERE rn > $1 AND completed_at IS NOT NULL AND completed_at < $2`

// CleanupOldRuns prunes completed/error/cancelled task runs (and their logs)
// older than retentionDays, ALWAYS preserving the most recent keepPerTask runs
// per task bucket (prompt+recurrence) regardless of age (#252). retentionDays<=0
// disables pruning (returns 0) so a misconfiguration can never mass-delete.
// Returns the number of task rows deleted.
func (db *Database) CleanupOldRuns(ctx context.Context, retentionDays, keepPerTask int) (int, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	if keepPerTask < 0 {
		keepPerTask = 0
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM run_logs WHERE task_id IN (`+cleanupEligibleSubquery+`)`,
		keepPerTask, cutoff); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM logs WHERE task_id IN (`+cleanupEligibleSubquery+`)`,
		keepPerTask, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM tasks WHERE id IN (`+cleanupEligibleSubquery+`)`,
		keepPerTask, cutoff)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(affected), nil
}

// DeleteOldHistory deletes tasks and logs older than days, in one transaction.
func (db *Database) DeleteOldHistory(ctx context.Context, days int) (int, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days)

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	// Rollback is a no-op after a successful Commit (returns sql.ErrTxDone); on
	// the error paths the function already returns the underlying error, and a
	// rollback failure in a defer can't be surfaced — so the result is
	// intentionally ignored.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM run_logs WHERE task_id IN (
			SELECT id FROM tasks
			WHERE status IN ($1, $2, $3) AND completed_at < $4
		)`,
		string(models.TaskStatusSuccess),
		string(models.TaskStatusError),
		string(models.TaskStatusCancelled),
		cutoff,
	); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM logs WHERE task_id IN (
			SELECT id FROM tasks
			WHERE status IN ($1, $2, $3) AND completed_at < $4
		)`,
		string(models.TaskStatusSuccess),
		string(models.TaskStatusError),
		string(models.TaskStatusCancelled),
		cutoff,
	); err != nil {
		return 0, err
	}

	res, err := tx.ExecContext(ctx, `
		DELETE FROM tasks
		WHERE status IN ($1, $2, $3) AND completed_at < $4`,
		string(models.TaskStatusSuccess),
		string(models.TaskStatusError),
		string(models.TaskStatusCancelled),
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(affected), nil
}

// logArchiveCandidate is one live log payload eligible for archival.
type logArchiveCandidate struct {
	taskID uuid.UUID
	raw    []byte
}

// archiveCandidatesPage reads one keyset page of live (un-archived) log
// payloads for terminal tasks completed before cutoff. The cursor is fully
// drained and closed before return so the caller can UPDATE on the same
// (possibly single-conn) pool without deadlocking. after, when non-nil,
// is the exclusive lower bound on task_id (#1122).
func (db *Database) archiveCandidatesPage(ctx context.Context, cutoff time.Time, after *uuid.UUID, limit int) ([]logArchiveCandidate, error) {
	q := `
		SELECT l.task_id, l.session_data
		FROM logs l
		JOIN tasks t ON t.id = l.task_id
		WHERE t.status IN ($1, $2, $3)
		  AND t.completed_at < $4
		  AND l.session_data IS NOT NULL
		  AND l.session_compression IS NULL`
	args := []any{
		string(models.TaskStatusSuccess),
		string(models.TaskStatusError),
		string(models.TaskStatusCancelled),
		cutoff,
	}
	if after != nil {
		q += ` AND l.task_id > $5 ORDER BY l.task_id LIMIT $6`
		args = append(args, *after, limit)
	} else {
		q += ` ORDER BY l.task_id LIMIT $5`
		args = append(args, limit)
	}
	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []logArchiveCandidate
	for rows.Next() {
		var taskID uuid.UUID
		var sessionData string
		if err := rows.Scan(&taskID, &sessionData); err != nil {
			return nil, err
		}
		candidates = append(candidates, logArchiveCandidate{taskID: taskID, raw: []byte(sessionData)})
	}
	return candidates, rows.Err()
}

// ArchiveOldLogs compresses (and, when an archive key is configured, AES-256-GCM
// encrypts) the session_data payload of completed-task logs older than `days`,
// IN PLACE (#272): the payload moves into session_data_gz and session_data is
// nulled in a single per-row UPDATE. Only terminal tasks (success/error/
// cancelled) with a live payload are touched; already-archived rows
// (session_compression set) are skipped, so the sweep is idempotent. days<=0
// disables archival and returns (0, 0, nil) so a misconfiguration is inert.
//
// It returns the number of rows archived and the total bytes saved (the sum of
// raw-minus-stored sizes; ~always positive for real log payloads). Each row is
// committed independently: a row's archive write and its DB update are one
// statement, so there is no window where the payload exists in neither column.
// The per-row UPDATE inside the page loop is deliberate and is NOT an N+1 that
// batching would fix: every row carries a DIFFERENT payload compressed (and
// optionally encrypted) in Go, so there is nothing to join against, and folding
// a page of multi-megabyte blobs into one statement would trade the per-row
// commit boundary for a large memory spike.
// Candidates are fetched in keyset pages of archiveScanChunk so first-run
// archival of a large table stays memory-bounded (#1122).
func (db *Database) ArchiveOldLogs(ctx context.Context, days int) (int, int64, error) {
	if days <= 0 {
		return 0, 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)

	var archived int
	var bytesSaved int64
	var after *uuid.UUID
	for {
		candidates, err := db.archiveCandidatesPage(ctx, cutoff, after, archiveScanChunk)
		if err != nil {
			return archived, bytesSaved, err
		}
		if len(candidates) == 0 {
			return archived, bytesSaved, nil
		}
		for _, c := range candidates {
			stored, codec, err := encodeArchive(c.raw, db.archiveKey)
			if err != nil {
				return archived, bytesSaved, err
			}
			// One statement flips the row from live to archived: set the compressed
			// payload + codec and null session_data together. The guard re-checks
			// session_compression IS NULL so two concurrent sweeps can't double-archive.
			res, err := db.conn.ExecContext(ctx, `
				UPDATE logs
				SET session_data = NULL, session_data_gz = $1, session_compression = $2
				WHERE task_id = $3 AND session_compression IS NULL`,
				stored, codec, c.taskID)
			if err != nil {
				return archived, bytesSaved, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue // raced by another sweep; leave counters untouched
			}
			archived++
			bytesSaved += int64(len(c.raw) - len(stored))
			id := c.taskID
			after = &id
		}
		if len(candidates) < archiveScanChunk {
			return archived, bytesSaved, nil
		}
	}
}
