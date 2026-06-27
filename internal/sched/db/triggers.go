package db

import (
	"context"
	"database/sql"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/google/uuid"
)

// triggerColumns is the shared column list for task_triggers scans (#177).
const triggerColumns = "id, task_id, slug, secret, prompt_template, created_at, updated_at"

func scanTrigger(scanner interface{ Scan(...interface{}) error }) (*models.TaskTrigger, error) {
	var t models.TaskTrigger
	if err := scanner.Scan(&t.ID, &t.TaskID, &t.Slug, &t.Secret, &t.PromptTemplate, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateTrigger inserts a webhook trigger. The slug is UNIQUE; a collision
// surfaces as a constraint error the caller can map to a 409.
func (db *Database) CreateTrigger(ctx context.Context, t *models.TaskTrigger) error {
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO task_triggers (id, task_id, slug, secret, prompt_template)
		VALUES ($1, $2, $3, $4, $5)`,
		t.ID, t.TaskID, t.Slug, t.Secret, t.PromptTemplate)
	return err
}

// GetTriggerBySlug looks up a trigger by its URL slug. Returns sql.ErrNoRows
// when no trigger matches — the webhook handler maps that to 401 (not 404) to
// avoid slug enumeration.
func (db *Database) GetTriggerBySlug(ctx context.Context, slug string) (*models.TaskTrigger, error) {
	row := db.conn.QueryRowContext(ctx,
		"SELECT "+triggerColumns+" FROM task_triggers WHERE slug = $1", slug)
	return scanTrigger(row)
}

// GetTrigger looks up a trigger by its ID.
func (db *Database) GetTrigger(ctx context.Context, id uuid.UUID) (*models.TaskTrigger, error) {
	row := db.conn.QueryRowContext(ctx,
		"SELECT "+triggerColumns+" FROM task_triggers WHERE id = $1", id)
	return scanTrigger(row)
}

// ListTriggers returns triggers, optionally filtered to a single task.
func (db *Database) ListTriggers(ctx context.Context, taskID *uuid.UUID) ([]*models.TaskTrigger, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if taskID != nil {
		rows, err = db.conn.QueryContext(ctx,
			"SELECT "+triggerColumns+" FROM task_triggers WHERE task_id = $1 ORDER BY created_at ASC", *taskID)
	} else {
		rows, err = db.conn.QueryContext(ctx,
			"SELECT "+triggerColumns+" FROM task_triggers ORDER BY created_at ASC")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.TaskTrigger, 0)
	for rows.Next() {
		t, serr := scanTrigger(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTrigger removes a trigger by ID, reporting whether a row was deleted.
func (db *Database) DeleteTrigger(ctx context.Context, id uuid.UUID) (bool, error) {
	res, err := db.conn.ExecContext(ctx, "DELETE FROM task_triggers WHERE id = $1", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RotateTriggerSecret replaces a trigger's HMAC secret, reporting whether a row
// matched. The old secret is invalidated immediately.
func (db *Database) RotateTriggerSecret(ctx context.Context, id uuid.UUID, secret string) (bool, error) {
	res, err := db.conn.ExecContext(ctx,
		"UPDATE task_triggers SET secret = $1, updated_at = now() WHERE id = $2", secret, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
