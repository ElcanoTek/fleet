package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/google/uuid"
)

// triggerColumns is the shared column list for task_triggers scans (#177, +kind
// and email_policy in #511).
const triggerColumns = "id, task_id, slug, secret, prompt_template, kind, email_policy, created_at, updated_at"

func scanTrigger(scanner interface{ Scan(...interface{}) error }) (*models.TaskTrigger, error) {
	var (
		t           models.TaskTrigger
		kind        sql.NullString
		emailPolicy sql.NullString
	)
	if err := scanner.Scan(&t.ID, &t.TaskID, &t.Slug, &t.Secret, &t.PromptTemplate, &kind, &emailPolicy, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.Kind = models.TriggerKind(kind.String)
	if t.Kind == "" {
		t.Kind = models.TriggerKindWebhook
	}
	t.EmailPolicy = unmarshalEmailPolicy(emailPolicy)
	return &t, nil
}

// marshalEmailPolicy serializes the optional email_policy JSONB column: nil → SQL
// NULL, non-nil → the JSON bytes (mirrors marshalSandboxLimits).
func marshalEmailPolicy(p *models.EmailTriggerPolicy) any {
	if p == nil {
		return nil
	}
	return marshalJSON(p)
}

// unmarshalEmailPolicy reads the nullable email_policy column back. NULL/empty → nil.
func unmarshalEmailPolicy(ns sql.NullString) *models.EmailTriggerPolicy {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var p models.EmailTriggerPolicy
	if err := json.Unmarshal([]byte(ns.String), &p); err != nil {
		log.Printf("Warning: failed to unmarshal email_policy: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return &p
}

// CreateTrigger inserts a webhook/email trigger. The slug is UNIQUE; a collision
// surfaces as a constraint error the caller can map to a 409.
func (db *Database) CreateTrigger(ctx context.Context, t *models.TaskTrigger) error {
	kind := t.KindOrWebhook()
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO task_triggers (id, task_id, slug, secret, prompt_template, kind, email_policy)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		t.ID, t.TaskID, t.Slug, t.Secret, t.PromptTemplate, string(kind), marshalEmailPolicy(t.EmailPolicy))
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

// RecordTriggerEvent inserts one accepted inbound event, enforcing idempotency:
// a duplicate (trigger_id, idempotency_key) is a NO-OP that returns inserted=false
// (the second delivery of the same email must not spawn a second run). On a fresh
// insert it returns inserted=true and populates ev.ID/ev.CreatedAt.
func (db *Database) RecordTriggerEvent(ctx context.Context, ev *models.TriggerEvent) (bool, error) {
	if ev.ID == uuid.Nil {
		ev.ID = uuid.New()
	}
	row := db.conn.QueryRowContext(ctx, `
		INSERT INTO trigger_events (id, trigger_id, idempotency_key, sender, subject, message_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (trigger_id, idempotency_key) DO NOTHING
		RETURNING id, created_at`,
		ev.ID, ev.TriggerID, ev.IdempotencyKey, ev.Sender, ev.Subject, ev.MessageID)
	err := row.Scan(&ev.ID, &ev.CreatedAt)
	if err == sql.ErrNoRows {
		// Conflict: the event was already recorded — a duplicate delivery.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// SetTriggerEventRunID links an accepted event to the run it spawned, so the run
// can be traced back to its originating event (and reply-back can find the reply
// target). Best-effort: a missing row is not an error.
func (db *Database) SetTriggerEventRunID(ctx context.Context, eventID, runID uuid.UUID) error {
	_, err := db.conn.ExecContext(ctx,
		"UPDATE trigger_events SET run_id = $1 WHERE id = $2", runID, eventID)
	return err
}

// GetTriggerEventByRunID returns the inbound event a given run answers, or
// sql.ErrNoRows when the run did not originate from a trigger event. Used by the
// reply-back path to recover the original sender + message-id.
func (db *Database) GetTriggerEventByRunID(ctx context.Context, runID uuid.UUID) (*models.TriggerEvent, error) {
	row := db.conn.QueryRowContext(ctx, `
		SELECT id, trigger_id, idempotency_key, sender, subject, message_id, run_id, created_at
		FROM trigger_events WHERE run_id = $1`, runID)
	var ev models.TriggerEvent
	var runIDCol uuid.NullUUID
	if err := row.Scan(&ev.ID, &ev.TriggerID, &ev.IdempotencyKey, &ev.Sender, &ev.Subject, &ev.MessageID, &runIDCol, &ev.CreatedAt); err != nil {
		return nil, err
	}
	if runIDCol.Valid {
		ev.RunID = &runIDCol.UUID
	}
	return &ev, nil
}
