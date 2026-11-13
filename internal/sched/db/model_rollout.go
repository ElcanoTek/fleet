package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

const recurrenceRolloutKeyPrefix = "sched_recurrence_rollout:"

func recurrenceRolloutKey(taskID uuid.UUID) string {
	return recurrenceRolloutKeyPrefix + taskID.String()
}

// RecurrenceModelOverride records a pending primary/fallback model upgrade for
// a recurring task occurrence that was already in flight (leased, running,
// paused, or awaiting recurrence spawn) when an operator model rollout ran.
// Persisted in chat.workspace_settings inside the rollout transaction so it is
// atomic with unstarted task updates, survives server restarts, and applies
// only to the exact occurrence that missed the direct tasks-row update without
// overriding later task edits.
type RecurrenceModelOverride struct {
	HasModel      bool    `json:"has_model"`
	Model         *string `json:"model,omitempty"`
	HasFallback   bool    `json:"has_fallback"`
	FallbackModel *string `json:"fallback_model,omitempty"`
}

// ApplyToPointers overlays any recorded primary/fallback model override onto a
// recurring task's cloned next-occurrence pointers.
func (o RecurrenceModelOverride) ApplyToPointers(curModel, curFallback *string) (nextModel, nextFallback *string) {
	nextModel = cloneNormalizedOptStr(curModel)
	nextFallback = cloneNormalizedOptStr(curFallback)
	if o.HasModel {
		nextModel = cloneNormalizedOptStr(o.Model)
	}
	if o.HasFallback {
		nextFallback = cloneNormalizedOptStr(o.FallbackModel)
	}
	return nextModel, nextFallback
}

func cloneNormalizedOptStr(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	if v == "" {
		return nil
	}
	return &v
}

func optStrEqual(a, b *string) bool {
	var av, bv string
	if a != nil {
		av = strings.TrimSpace(*a)
	}
	if b != nil {
		bv = strings.TrimSpace(*b)
	}
	return av == bv
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func chatWorkspaceSettingsReady(ctx context.Context, q rowQuerier) (bool, error) {
	var ready bool
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('chat.workspace_settings') IS NOT NULL`).Scan(&ready); err != nil {
		return false, err
	}
	return ready, nil
}

// ClearRecurrenceModelOverrideTx deletes any pending recurrence model override
// for taskID inside tx (called when an editable task is modified so post-rollout
// operator edits take precedence).
func (db *Database) ClearRecurrenceModelOverrideTx(ctx context.Context, tx *sql.Tx, taskID uuid.UUID) error {
	ready, err := chatWorkspaceSettingsReady(ctx, tx)
	if err != nil || !ready {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM chat.workspace_settings WHERE key = $1`, recurrenceRolloutKey(taskID))
	return err
}

// ConsumeRecurrenceModelOverrideTx atomically deletes and returns any pending
// recurrence model override for taskID inside scheduleNextRecurrence's spawn
// transaction. If the spawn transaction rolls back, the override remains intact
// for ReconcileRecurrences.
func (db *Database) ConsumeRecurrenceModelOverrideTx(ctx context.Context, tx *sql.Tx, taskID uuid.UUID) (RecurrenceModelOverride, bool, error) {
	ready, err := chatWorkspaceSettingsReady(ctx, tx)
	if err != nil || !ready {
		return RecurrenceModelOverride{}, false, err
	}
	var raw string
	err = tx.QueryRowContext(ctx,
		`DELETE FROM chat.workspace_settings WHERE key = $1 RETURNING value`,
		recurrenceRolloutKey(taskID),
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return RecurrenceModelOverride{}, false, nil
	}
	if err != nil {
		return RecurrenceModelOverride{}, false, err
	}
	var ov RecurrenceModelOverride
	if err := json.Unmarshal([]byte(raw), &ov); err != nil {
		return RecurrenceModelOverride{}, false, fmt.Errorf("decode recurrence model override for %s: %w", taskID, err)
	}
	return ov, true, nil
}

func loadRecurrenceModelOverrides(ctx context.Context, q rowQuerier, forUpdate bool) (map[uuid.UUID]RecurrenceModelOverride, bool, error) {
	ready, err := chatWorkspaceSettingsReady(ctx, q)
	if err != nil || !ready {
		return nil, ready, err
	}
	stmt := `SELECT key, value FROM chat.workspace_settings WHERE key LIKE 'sched_recurrence_rollout:%'`
	if forUpdate {
		stmt += ` FOR UPDATE`
	}
	rows, err := q.QueryContext(ctx, stmt)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]RecurrenceModelOverride)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, true, err
		}
		idRaw := strings.TrimPrefix(k, recurrenceRolloutKeyPrefix)
		id, err := uuid.Parse(idRaw)
		if err != nil {
			continue
		}
		var ov RecurrenceModelOverride
		if err := json.Unmarshal([]byte(v), &ov); err != nil {
			continue
		}
		out[id] = ov
	}
	return out, true, rows.Err()
}

func upsertRecurrenceModelOverrideTx(ctx context.Context, tx *sql.Tx, taskID uuid.UUID, ov RecurrenceModelOverride) error {
	payload, err := json.Marshal(ov)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO chat.workspace_settings (key, value, updated_at, updated_by)
		VALUES ($1, $2, $3, 'model_rollout')
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
		recurrenceRolloutKey(taskID), string(payload), time.Now().Unix(),
	)
	return err
}

func rolloutCandidateQuery(recurringOnly, forUpdate bool) string {
	dispatchPred := `status IN ($1, $2)`
	if recurringOnly {
		dispatchPred += ` AND COALESCE(recurrence, '') <> ''`
	}
	inFlightRecurrencePred := `(COALESCE(recurrence, '') <> '' AND NOT recurrence_spawned AND status NOT IN ($1, $2, $3))`
	q := `SELECT ` + taskColumns + ` FROM tasks WHERE (` + dispatchPred + `) OR ` + inFlightRecurrencePred + ` ORDER BY created_at ASC, id ASC`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	return q
}

func dispatchableRolloutQuery(recurringOnly bool) string {
	q := `SELECT ` + taskColumns + ` FROM tasks WHERE status IN ($1, $2)`
	if recurringOnly {
		q += ` AND COALESCE(recurrence, '') <> ''`
	}
	return q + ` ORDER BY created_at ASC, id ASC FOR UPDATE`
}

func updateTaskModelColumnsTx(ctx context.Context, tx *sql.Tx, taskID uuid.UUID, nextM, nextFB *string) error {
	var mVal, fbVal any
	if nextM != nil {
		mVal = nullableString(*nextM)
	}
	if nextFB != nil {
		fbVal = nullableString(*nextFB)
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE tasks SET model = $1, fallback_model = $2 WHERE id = $3`,
		mVal, fbVal, taskID,
	)
	return err
}

func previewRolloutTasks(
	tasks []*models.Task,
	existing map[uuid.UUID]RecurrenceModelOverride,
	opts models.TaskModelRollout,
) models.TaskModelRolloutResult {
	res := models.TaskModelRolloutResult{DryRun: true, ScannedCount: len(tasks)}
	for _, t := range tasks {
		effM, effFB := t.Model, t.FallbackModel
		if ov, ok := existing[t.ID]; ok {
			effM, effFB = ov.ApplyToPointers(effM, effFB)
		}
		_, _, pChanged, fbChanged := opts.ApplyToPointers(effM, effFB)
		if !pChanged && !fbChanged {
			continue
		}
		res.UpdatedCount++
		if pChanged {
			res.PrimaryUpdatedCount++
		}
		if fbChanged {
			res.FallbackUpdatedCount++
		}
		if strings.TrimSpace(t.Recurrence) != "" {
			res.RecurringUpdatedCount++
		}
		preview := *t
		preview.Model = effM
		preview.FallbackModel = effFB
		res.Tasks = append(res.Tasks, &preview)
	}
	return res
}

func applyRolloutTaskRowTx(
	ctx context.Context,
	tx *sql.Tx,
	wsReady bool,
	existing map[uuid.UUID]RecurrenceModelOverride,
	t *models.Task,
	opts models.TaskModelRollout,
	res *models.TaskModelRolloutResult,
) error {
	ov, hadOv := existing[t.ID]
	effM, effFB := t.Model, t.FallbackModel
	if hadOv {
		effM, effFB = ov.ApplyToPointers(effM, effFB)
	}
	nextM, nextFB, pChanged, fbChanged := opts.ApplyToPointers(effM, effFB)

	if liveTaskStatus(t.Status) {
		rowPChanged := !optStrEqual(t.Model, nextM)
		rowFBChanged := !optStrEqual(t.FallbackModel, nextFB)
		if !rowPChanged && !rowFBChanged && !hadOv {
			return nil
		}
		if rowPChanged || rowFBChanged {
			if err := updateTaskModelColumnsTx(ctx, tx, t.ID, nextM, nextFB); err != nil {
				return err
			}
		}
		if hadOv && wsReady {
			if _, err := tx.ExecContext(ctx, `DELETE FROM chat.workspace_settings WHERE key = $1`, recurrenceRolloutKey(t.ID)); err != nil {
				return err
			}
		}
		if pChanged || fbChanged {
			recordRolloutCounts(res, pChanged, fbChanged, t.Recurrence)
		}
		return nil
	}

	if !pChanged && !fbChanged {
		return nil
	}
	if !wsReady {
		return errors.New("chat.workspace_settings table unavailable for durable recurring task rollout")
	}
	if pChanged {
		ov.HasModel = true
		ov.Model = cloneNormalizedOptStr(nextM)
	}
	if fbChanged {
		ov.HasFallback = true
		ov.FallbackModel = cloneNormalizedOptStr(nextFB)
	}
	if err := upsertRecurrenceModelOverrideTx(ctx, tx, t.ID, ov); err != nil {
		return err
	}
	recordRolloutCounts(res, pChanged, fbChanged, t.Recurrence)
	return nil
}

func recordRolloutCounts(res *models.TaskModelRolloutResult, pChanged, fbChanged bool, recurrence string) {
	res.UpdatedCount++
	if pChanged {
		res.PrimaryUpdatedCount++
	}
	if fbChanged {
		res.FallbackUpdatedCount++
	}
	if strings.TrimSpace(recurrence) != "" {
		res.RecurringUpdatedCount++
	}
}

func pruneOrphanRecurrenceModelOverridesTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM chat.workspace_settings ws
		WHERE ws.key LIKE 'sched_recurrence_rollout:%'
		  AND NOT EXISTS (
			SELECT 1 FROM tasks t
			WHERE ('sched_recurrence_rollout:' || t.id::text) = ws.key
			  AND COALESCE(t.recurrence, '') <> ''
			  AND NOT t.recurrence_spawned
			  AND t.status NOT IN ($1, $2, $3)
		  )`,
		string(models.TaskStatusScheduled),
		string(models.TaskStatusPending),
		string(models.TaskStatusCancelled),
	)
	return err
}

// RolloutTaskModels upgrades the primary model and/or fallback_model across
// upcoming unstarted tasks (scheduled and pending, optionally restricted to
// recurring cron tasks) and durably records pending recurrence overrides for
// any recurring tasks currently mid-run so their next spawned occurrence
// inherits the rollout even across restarts, without mutating completed/running
// history or clobbering subsequent task edits.
func (db *Database) RolloutTaskModels(ctx context.Context, opts models.TaskModelRollout, dryRun bool) (models.TaskModelRolloutResult, error) {
	q := rolloutCandidateQuery(opts.RecurringOnly, !dryRun)
	args := []any{
		string(models.TaskStatusScheduled),
		string(models.TaskStatusPending),
		string(models.TaskStatusCancelled),
	}

	if dryRun {
		existing, _, err := loadRecurrenceModelOverrides(ctx, db.conn, false)
		if err != nil {
			return models.TaskModelRolloutResult{DryRun: true}, err
		}
		rows, err := db.conn.QueryContext(ctx, q, args...)
		if err != nil {
			return models.TaskModelRolloutResult{DryRun: true}, err
		}
		defer rows.Close()
		tasks, err := db.rowsToTasks(ctx, rows)
		if err != nil {
			return models.TaskModelRolloutResult{DryRun: true}, err
		}
		return previewRolloutTasks(tasks, existing, opts), nil
	}

	var res models.TaskModelRolloutResult
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	existing, wsReady, err := loadRecurrenceModelOverrides(ctx, tx, true)
	if err != nil {
		return res, err
	}

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return res, err
	}
	tasks, err := db.rowsToTasks(ctx, rows)
	_ = rows.Close()
	if err != nil {
		return res, err
	}

	seen := make(map[uuid.UUID]struct{}, len(tasks))
	res.ScannedCount = len(tasks)
	for _, t := range tasks {
		seen[t.ID] = struct{}{}
		if err := applyRolloutTaskRowTx(ctx, tx, wsReady, existing, t, opts, &res); err != nil {
			return res, err
		}
	}

	// Second pass catches any brand-new scheduled/pending successor inserted by
	// a concurrent scheduleNextRecurrence that committed while Pass 1 waited on
	// the completing predecessor's row lock.
	rows2, err := tx.QueryContext(ctx, dispatchableRolloutQuery(opts.RecurringOnly),
		string(models.TaskStatusScheduled), string(models.TaskStatusPending))
	if err != nil {
		return res, err
	}
	extraTasks, err := db.rowsToTasks(ctx, rows2)
	_ = rows2.Close()
	if err != nil {
		return res, err
	}
	for _, t := range extraTasks {
		if _, ok := seen[t.ID]; ok {
			continue
		}
		res.ScannedCount++
		if err := applyRolloutTaskRowTx(ctx, tx, wsReady, existing, t, opts, &res); err != nil {
			return res, err
		}
	}

	if wsReady {
		if err := pruneOrphanRecurrenceModelOverridesTx(ctx, tx); err != nil {
			return res, err
		}
	}

	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}
