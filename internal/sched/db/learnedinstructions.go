package db

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Self-improving memory accessors (#516): task feedback signals + the
// versioned/revertible learned instructions distilled from them.

// AddTaskFeedback records one signal.
func (db *Database) AddTaskFeedback(ctx context.Context, f *models.TaskFeedback) error {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO task_feedback (id, task_id, rating, critique, created_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		f.ID, f.TaskID, f.Rating, f.Critique, f.CreatedAt, f.CreatedBy)
	return err
}

// UnconsumedFeedback returns the fresh (not-yet-distilled) signals for a task,
// oldest first.
func (db *Database) UnconsumedFeedback(ctx context.Context, taskID uuid.UUID) ([]*models.TaskFeedback, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, task_id, rating, critique, consumed, created_at, created_by
		FROM task_feedback WHERE task_id = $1 AND consumed = FALSE
		ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*models.TaskFeedback
	for rows.Next() {
		var f models.TaskFeedback
		if err := rows.Scan(&f.ID, &f.TaskID, &f.Rating, &f.Critique, &f.Consumed, &f.CreatedAt, &f.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, &f)
	}
	return out, rows.Err()
}

// markFeedbackConsumed flips the given signals to consumed within a tx.
func markFeedbackConsumed(ctx context.Context, tx *sql.Tx, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE task_feedback SET consumed = TRUE WHERE id = ANY($1::uuid[])`, pqUUIDArray(ids))
	return err
}

const learnedColumns = "id, task_id, version, content, status, signal_count, created_at, activated_at, activated_by"

func scanLearned(scanner interface{ Scan(...interface{}) error }) (*models.TaskLearnedInstruction, error) {
	var (
		li  models.TaskLearnedInstruction
		act sql.NullInt64
	)
	if err := scanner.Scan(&li.ID, &li.TaskID, &li.Version, &li.Content, &li.Status,
		&li.SignalCount, &li.CreatedAt, &act, &li.ActivatedBy); err != nil {
		return nil, err
	}
	if act.Valid {
		li.ActivatedAt = &act.Int64
	}
	return &li, nil
}

// ProposeLearnedInstruction inserts a distilled instruction at the next version
// for the task, marks the given evidence signals consumed, and returns the new
// row — all in one tx so a crash never leaves signals consumed without a
// proposal (or vice-versa). Status is 'proposed' (enterprise-staged): it does
// NOT change behavior until activated.
func (db *Database) ProposeLearnedInstruction(ctx context.Context, taskID uuid.UUID, content string, evidenceIDs []uuid.UUID, now int64) (*models.TaskLearnedInstruction, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM task_learned_instructions WHERE task_id = $1`, taskID,
	).Scan(&version); err != nil {
		return nil, err
	}
	id := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_learned_instructions (id, task_id, version, content, status, signal_count, created_at)
		VALUES ($1, $2, $3, $4, 'proposed', $5, $6)`,
		id, taskID, version, content, len(evidenceIDs), now); err != nil {
		return nil, err
	}
	if err := markFeedbackConsumed(ctx, tx, evidenceIDs); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &models.TaskLearnedInstruction{
		ID: id, TaskID: taskID, Version: version, Content: content,
		Status: models.LearnedProposed, SignalCount: len(evidenceIDs), CreatedAt: now,
	}, nil
}

// ListLearnedInstructions returns a task's instructions, newest version first.
func (db *Database) ListLearnedInstructions(ctx context.Context, taskID uuid.UUID) ([]*models.TaskLearnedInstruction, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT `+learnedColumns+` FROM task_learned_instructions WHERE task_id = $1 ORDER BY version DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*models.TaskLearnedInstruction
	for rows.Next() {
		li, err := scanLearned(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, li)
	}
	return out, rows.Err()
}

// ActiveLearnedInstruction returns the task's active instruction, or nil. This
// is what the scheduled run injects at prompt-assembly time.
func (db *Database) ActiveLearnedInstruction(ctx context.Context, taskID uuid.UUID) (*models.TaskLearnedInstruction, error) {
	row := db.conn.QueryRowContext(ctx,
		`SELECT `+learnedColumns+` FROM task_learned_instructions WHERE task_id = $1 AND status = 'active'`, taskID)
	li, err := scanLearned(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return li, nil
}

// ActivateLearnedInstruction makes version the task's sole active instruction
// (archiving any prior active), in one tx. This is BOTH the activate and the
// revert primitive: reverting is activating an older version. Returns the
// activated row.
func (db *Database) ActivateLearnedInstruction(ctx context.Context, taskID uuid.UUID, version int, who string, now int64) (*models.TaskLearnedInstruction, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Archive the current active (if any) FIRST so the partial unique index
	// never sees two active rows mid-transaction.
	if _, err := tx.ExecContext(ctx,
		`UPDATE task_learned_instructions SET status = 'archived' WHERE task_id = $1 AND status = 'active'`, taskID); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE task_learned_instructions
		SET status = 'active', activated_at = $1, activated_by = $2
		WHERE task_id = $3 AND version = $4`,
		now, who, taskID, version)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, errors.New("learned instruction version not found")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.getLearnedByVersion(ctx, taskID, version)
}

// DeactivateLearnedInstructions archives the task's active instruction (a full
// revert to "no learned instruction"). Returns whether one was active.
func (db *Database) DeactivateLearnedInstructions(ctx context.Context, taskID uuid.UUID) (bool, error) {
	res, err := db.conn.ExecContext(ctx,
		`UPDATE task_learned_instructions SET status = 'archived' WHERE task_id = $1 AND status = 'active'`, taskID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (db *Database) getLearnedByVersion(ctx context.Context, taskID uuid.UUID, version int) (*models.TaskLearnedInstruction, error) {
	row := db.conn.QueryRowContext(ctx,
		`SELECT `+learnedColumns+` FROM task_learned_instructions WHERE task_id = $1 AND version = $2`, taskID, version)
	return scanLearned(row)
}
