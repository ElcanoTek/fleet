package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/pgtext"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/google/uuid"
)

// triggerColumns is the shared column list for task_triggers scans (#177, +kind
// and email_policy in #511).
const triggerColumns = "id, task_id, slug, secret, prompt_template, kind, email_policy, created_at, updated_at"

func scanTrigger(ctx context.Context, scanner interface{ Scan(...interface{}) error }) (*models.TaskTrigger, error) {
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
	t.EmailPolicy = unmarshalEmailPolicy(ctx, emailPolicy)
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
func unmarshalEmailPolicy(ctx context.Context, ns sql.NullString) *models.EmailTriggerPolicy {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var p models.EmailTriggerPolicy
	if err := json.Unmarshal([]byte(ns.String), &p); err != nil {
		logging.From(ctx).Warn("Warning: failed to unmarshal email_policy", "err", err, "input", fmt.Sprintf("%.100s", ns.String))
		return nil
	}
	return &p
}

// CreateTrigger inserts a webhook/email trigger. The slug is UNIQUE; a collision
// surfaces as a constraint error the caller can map to a 409. The prompt
// template is content and loses U+0000 (pgtext.Content); the slug and secret
// are keys and are stored as given.
func (db *Database) CreateTrigger(ctx context.Context, t *models.TaskTrigger) error {
	kind := t.KindOrWebhook()
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO task_triggers (id, task_id, slug, secret, prompt_template, kind, email_policy)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		t.ID, t.TaskID, t.Slug, t.Secret, pgtext.Content(t.PromptTemplate), string(kind), marshalEmailPolicy(t.EmailPolicy))
	return err
}

// GetTriggerBySlug looks up the trigger with this URL slug AND this kind.
// Returns sql.ErrNoRows when no trigger matches, including a trigger that has
// the slug but another kind, so an ingress route sees another kind's slug
// exactly as an unknown one; the handlers map that to 401 (not 404) to avoid
// slug enumeration. The kind is filtered in the query, so an other-kind slug
// comes back as no row and the handler takes its unknown-slug path.
//
// kind is required, not an optional filter: each ingress route fires one kind
// of trigger, so the lookup names it. An invalid kind is an error, never a
// match-anything.
func (db *Database) GetTriggerBySlug(ctx context.Context, kind models.TriggerKind, slug string) (*models.TaskTrigger, error) {
	if !kind.IsValid() {
		return nil, fmt.Errorf("trigger lookup: %q is not a trigger kind", kind)
	}
	// A blank stored kind reads as webhook, the same default scanTrigger applies.
	row := db.conn.QueryRowContext(ctx,
		"SELECT "+triggerColumns+" FROM task_triggers WHERE slug = $1 AND COALESCE(NULLIF(kind, ''), $3) = $2",
		slug, string(kind), string(models.TriggerKindWebhook))
	return scanTrigger(ctx, row)
}

// GetTrigger looks up a trigger by its ID.
func (db *Database) GetTrigger(ctx context.Context, id uuid.UUID) (*models.TaskTrigger, error) {
	row := db.conn.QueryRowContext(ctx,
		"SELECT "+triggerColumns+" FROM task_triggers WHERE id = $1", id)
	return scanTrigger(ctx, row)
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
		t, serr := scanTrigger(ctx, rows)
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

// RecordTriggerEventTx inserts one inbound event inside tx, already linked to
// the run it starts (ev.RunID), enforcing idempotency: a duplicate (trigger_id,
// idempotency_key) is a NO-OP that returns inserted=false (the second delivery
// of the same email must not spawn a second run). On a fresh insert it returns
// inserted=true and populates ev.ID/ev.CreatedAt. The subject is the email's
// free text and loses U+0000; the idempotency key, sender and message id are
// keys and are stored as given.
//
// There is deliberately no form without a transaction: the caller creates the
// run in the same one, so the event and its run commit together or not at all.
// An event committed on its own marks the delivery seen even when its run is
// never created, and the sender's retry is then answered as a duplicate. While
// tx is open, a concurrent insert of the same key waits for it, then inserts if
// tx rolled back or reports inserted=false if tx committed.
func (db *Database) RecordTriggerEventTx(ctx context.Context, tx *sql.Tx, ev *models.TriggerEvent) (bool, error) {
	if ev.ID == uuid.Nil {
		ev.ID = uuid.New()
	}
	var runID uuid.NullUUID
	if ev.RunID != nil {
		runID = uuid.NullUUID{UUID: *ev.RunID, Valid: true}
	}
	row := tx.QueryRowContext(ctx, `
		INSERT INTO trigger_events (id, trigger_id, idempotency_key, sender, subject, message_id, run_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (trigger_id, idempotency_key) DO NOTHING
		RETURNING id, created_at`,
		ev.ID, ev.TriggerID, ev.IdempotencyKey, ev.Sender, pgtext.Content(ev.Subject), ev.MessageID, runID)
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

// GetTriggerEventByRunID returns the inbound event a given run answers, or
// sql.ErrNoRows when the run did not originate from a trigger event. Used by the
// reply-back path to recover the original sender + message-id.
func (db *Database) GetTriggerEventByRunID(ctx context.Context, runID uuid.UUID) (*models.TriggerEvent, error) {
	return scanTriggerEvent(db.conn.QueryRowContext(ctx,
		"SELECT "+triggerEventColumns+" FROM trigger_events WHERE run_id = $1", runID))
}

// GetTriggerEventByKey returns the recorded event with this idempotency key on
// this trigger, or sql.ErrNoRows when there is none. An event recorded now is
// final: it is committed only together with the run it started
// (RecordTriggerEventTx), so finding one means the delivery already ran. Rows
// recorded before that change, when the event committed on its own first, can
// have a NULL run_id from a delivery whose render, spawn or run link failed;
// they are found here too and answered as duplicates. No migration removes them.
func (db *Database) GetTriggerEventByKey(ctx context.Context, triggerID uuid.UUID, idempotencyKey string) (*models.TriggerEvent, error) {
	return scanTriggerEvent(db.conn.QueryRowContext(ctx,
		"SELECT "+triggerEventColumns+" FROM trigger_events WHERE trigger_id = $1 AND idempotency_key = $2", triggerID, idempotencyKey))
}

// triggerEventColumns is the shared column list for trigger_events scans.
const triggerEventColumns = "id, trigger_id, idempotency_key, sender, subject, message_id, run_id, created_at"

func scanTriggerEvent(row *sql.Row) (*models.TriggerEvent, error) {
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
