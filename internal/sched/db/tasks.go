package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Task operations.
//
// The tasks-table column lists (taskColumns, taskInsertColumns,
// taskInsertOnConflict, the UPDATE statement) and scanTask's positional scan
// all derive from taskColumnRegistry — see task_columns.go (#1126). A new
// task column is one migration + one registry row (+ the models.Task field).

// AddTask adds or updates a task via the registry-derived single-row upsert
// (taskInsertStatement, task_columns.go). The columns deliberately absent
// from the insert or its ON CONFLICT clause — effective_priority,
// recurrence_spawned, the result-like/pause/wake columns, and (since #1270)
// the creation-time created_by_key_id provenance — are declared, with
// per-column reasons, on their taskColumnRegistry rows.
//
// It stays an UNCONDITIONAL upsert: status and the lease columns ride the ON
// CONFLICT clause, so a caller that writes an id which already exists writes
// over live state. That verbatim behavior is load-bearing for same-generation
// re-import idempotency, so the operator import paths validate the collision
// at their OWN seam (internal/admincli/import_policy.go, #1267) rather than
// having AddTask second-guess its callers.
//
// taskInsertArgs populates actual_duration_seconds (#274) whenever a
// completion timestamp is present alongside a start, so EVERY write path
// that persists a completed_at also persists the derived actual, without
// each storage call site having to remember it. Idempotent: a pre-set value
// (e.g. a test seed) is left untouched.
func (db *Database) AddTask(ctx context.Context, task *models.Task) error {
	_, err := db.conn.ExecContext(ctx, taskInsertStatement, taskInsertArgs(task)...)
	return err
}

// AddTaskBatch inserts a slice of tasks in a single parameterised INSERT (#227),
// replacing N sequential ExecContext round-trips. It does NOT run inside an
// explicit transaction — callers that need atomicity wrap the call in BeginTx /
// Commit (see Storage.AddTaskBatch). An empty slice is a no-op.
//
// Each row carries the SAME registry-derived columns as AddTask (via the
// shared taskInsertArgs helper), so a row inserted through the batch path is
// byte-identical to one inserted through the single-row path. The placeholder
// count is len(taskInsertSet) — derived, never hand-maintained (#710's drift
// class is structurally gone).
func (db *Database) AddTaskBatch(ctx context.Context, tasks []*models.Task) error {
	return db.AddTaskBatchTx(ctx, nil, tasks)
}

// AddTaskBatchTx inserts a slice of tasks in a single parameterised INSERT within
// an existing transaction (#227), ensuring atomic multi-row insertions run in
// a single round-trip. An empty slice is a no-op.
func (db *Database) AddTaskBatchTx(ctx context.Context, tx *sql.Tx, tasks []*models.Task) error {
	if len(tasks) == 0 {
		return nil
	}

	cols := len(taskInsertSet)
	args := make([]any, 0, len(tasks)*cols)
	placeholders := make([]string, 0, len(tasks))
	var b strings.Builder
	for i, t := range tasks {
		base := i * cols
		b.Reset()
		b.WriteByte('(')
		for j := 0; j < cols; j++ {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "$%d", base+j+1)
		}
		b.WriteByte(')')
		placeholders = append(placeholders, b.String())
		args = append(args, taskInsertArgs(t)...)
	}

	var q strings.Builder
	q.WriteString("INSERT INTO tasks (")
	q.WriteString(taskInsertColumns)
	q.WriteString(") VALUES ")
	q.WriteString(strings.Join(placeholders, ","))
	q.WriteString(taskInsertOnConflict)

	var err error
	if tx != nil {
		_, err = tx.ExecContext(ctx, q.String(), args...)
	} else {
		_, err = db.conn.ExecContext(ctx, q.String(), args...)
	}
	return err
}

// AddTaskTx inserts a single task within an existing transaction. The atomic
// batch path (#227) uses this so a multi-row insert lands in the caller's tx.
// It executes the same registry-derived taskInsertStatement as AddTask.
func (db *Database) AddTaskTx(ctx context.Context, tx *sql.Tx, task *models.Task) error {
	_, err := tx.ExecContext(ctx, taskInsertStatement, taskInsertArgs(task)...)
	return err
}

// scanTask scans one tasks row into a models.Task. The scan destinations and
// the per-column conversions both come from taskColumnRegistry's read set —
// the SAME ordered slice taskColumns is joined from — so the SELECT list and
// the positional scan agree by construction (no manual ordering to drift).
// Hot path: per row it fills one destination slice and runs the assign
// functions; all statement text was built once at package init.
func (db *Database) scanTask(scanner interface{ Scan(...interface{}) error }) (*models.Task, error) {
	var buf taskScanBuf
	dests := make([]any, len(taskReadSet))
	for i, c := range taskReadSet {
		dests[i] = c.dest(&buf)
	}
	if err := scanner.Scan(dests...); err != nil {
		return nil, err
	}
	task := &models.Task{}
	for _, c := range taskReadSet {
		c.assign(&buf, task)
	}
	return task, nil
}

func (db *Database) rowsToTasks(rows *sql.Rows) ([]*models.Task, error) {
	tasks := make([]*models.Task, 0)
	for rows.Next() {
		task, err := db.scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// GetTask gets a task by ID.
func (db *Database) GetTask(ctx context.Context, taskID uuid.UUID) (*models.Task, error) {
	row := db.conn.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = $1", taskID)
	return db.scanTask(row)
}

// TaskExists reports whether a task row with the given id exists. Used by the
// legacy importer (docs/LEGACY-IMPORT.md) to make re-runs skip-by-default: a
// UUID already present in fleet is never overwritten unless the operator
// passes --overwrite, so a re-run can't revert live task state (#713).
func (db *Database) TaskExists(ctx context.Context, taskID uuid.UUID) (bool, error) {
	var exists bool
	err := db.conn.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM tasks WHERE id = $1)", taskID).Scan(&exists)
	return exists, err
}

// UpdateTask updates an existing task.
func (db *Database) UpdateTask(ctx context.Context, task *models.Task) error {
	return db.AddTask(ctx, task)
}

// UpdateTasksModelBatch updates the pinned model of scheduled tasks.
// fallbackModel is optional: nil leaves existing fallback_model values
// untouched; a non-nil empty string clears them to NULL; a non-nil
// non-empty string sets them. Callers must distinguish "flag not
// provided" from "explicitly clear" (#1120).
func (db *Database) UpdateTasksModelBatch(ctx context.Context, model string, fallbackModel *string, fromModel string) (int, error) {
	var res sql.Result
	var err error
	status := string(models.TaskStatusScheduled)
	switch {
	case fallbackModel == nil && fromModel != "":
		res, err = db.conn.ExecContext(ctx, `
			UPDATE tasks SET model = $1
			WHERE status = $2 AND model = $3`,
			model, status, fromModel)
	case fallbackModel == nil:
		res, err = db.conn.ExecContext(ctx, `
			UPDATE tasks SET model = $1
			WHERE status = $2`,
			model, status)
	case fromModel != "":
		res, err = db.conn.ExecContext(ctx, `
			UPDATE tasks SET model = $1, fallback_model = $2
			WHERE status = $3 AND model = $4`,
			model, nullableString(*fallbackModel), status, fromModel)
	default:
		res, err = db.conn.ExecContext(ctx, `
			UPDATE tasks SET model = $1, fallback_model = $2
			WHERE status = $3`,
			model, nullableString(*fallbackModel), status)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SetErrorAnalysis persists the post-failure LLM diagnosis (#317) as a narrow,
// lease-FREE single-column UPDATE. It runs in a detached goroutine AFTER the
// terminal-failure transition (which already released the lease), so it
// deliberately does not check lease ownership.
//
// The status guard (... AND status IN error/dead_lettered) makes the write a
// no-op once the row is no longer in a terminal-failure state — specifically, if
// an admin REPLAYED the dead-lettered task (same id → pending → running) while
// this analysis goroutine was still in flight, the stale diagnosis is dropped
// rather than stamped onto the fresh attempt. Writing a diagnostic annotation to
// a still-terminal-failed row is benign (touches neither status nor lease) and,
// like MarkSLABreached, the single-column write cannot race a broader row write.
// nil/empty raw → SQL NULL. Idempotent.
func (db *Database) SetErrorAnalysis(ctx context.Context, taskID uuid.UUID, raw json.RawMessage) error {
	_, err := db.conn.ExecContext(ctx,
		`UPDATE tasks SET error_analysis = $1 WHERE id = $2 AND status IN ($3, $4)`,
		marshalRawJSON(raw), taskID, string(models.TaskStatusError), string(models.TaskStatusDeadLettered))
	return err
}

// DeleteTask permanently removes one task and its transcripts, in a single
// transaction. See storage.DeleteTask for why deleting (not cancelling) is the
// only thing that frees a task's name.
//
// The two log tables are deleted explicitly because they hold a bare task_id
// with no foreign key (migrations 001 and 058); every other child table
// declares ON DELETE CASCADE. Ordered children-first so the task row is never
// orphaned mid-transaction.
func (db *Database) DeleteTask(ctx context.Context, taskID uuid.UUID) (bool, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	// A rollback after a successful Commit returns sql.ErrTxDone; the error
	// paths below already return the underlying failure.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM run_logs WHERE task_id = $1`, taskID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM logs WHERE task_id = $1`, taskID); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1`, taskID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return affected > 0, nil
}

// GetTaskForUpdate gets a task by ID with a row-level lock. Must be in a tx.
func (db *Database) GetTaskForUpdate(ctx context.Context, tx *sql.Tx, taskID uuid.UUID) (*models.Task, error) {
	row := tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = $1 FOR UPDATE", taskID)
	return db.scanTask(row)
}

// UpdateTaskTx updates a task within a transaction, via the registry-derived
// UPDATE statement (taskUpdateStatement, task_columns.go): id is the WHERE
// key, the txUpdate-flagged columns are SET in registry order. The columns
// deliberately absent (effective_priority, created_by_key_id, the wake/pause
// clocks, error_analysis, recurrence_spawned) are declared — with reasons —
// on their taskColumnRegistry rows. name, trigger_type, allow_event_triggers
// and serialization_key are in the set for import conflict=replace (#1104),
// the only tx write path that changes them: every other UpdateTaskTx caller
// writes back the values it scanned under the same row lock
// (GetTaskForUpdate / ClaimNextPendingTask), so for them these are no-op
// write-backs.
func (db *Database) UpdateTaskTx(ctx context.Context, tx *sql.Tx, task *models.Task) error {
	// Populate actual_duration_seconds (#274) on the same write that persists a
	// completed_at — mirrors AddTask so the storage call sites that go through
	// UpdateTaskTx (the terminal-status transitions) record the derived actual
	// without each one having to remember it.
	maybeComputeActualDuration(task)
	_, err := tx.ExecContext(ctx, taskUpdateStatement, updateTaskArgs(task)...)
	return err
}
