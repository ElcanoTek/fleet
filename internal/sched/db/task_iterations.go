package db

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// ── task_iterations (per-iteration telemetry, #179, and the task-side spend record) ──

const taskIterationColumns = "id, task_id, iteration_number, started_at, completed_at, worker_session_id, exit_condition_result, cost_usd, prompt_tokens, completion_tokens, status"

// AddTaskIteration inserts or updates a per-iteration telemetry row (upsert on
// id, so a row created at iteration start can be finalized at iteration end).
//
// The row is also the task-side spend record (docs/USAGE-ANALYTICS.md): the
// usage report, the adoption report and the budgets aggregate it, and it
// outlives its task (migration 0210 — no foreign key), so spend recorded for
// a task keeps counting after the task is deleted or pruned. It therefore
// carries the attribution those readers group by — the task's creator,
// submitting key and model — copied from the task row by the INSERT itself,
// so while the task row exists no caller of AddTaskIteration can leave it
// out; for a task already deleted, both production callers (the single-pass
// and the looped run in internal/scheduledrun) pass the runner's copy, below.
// The first write stamps
// it; the upsert never changes it (only storage.SetTaskOwner and
// SetTaskLineageOwner re-attribute it, together with the task).
//
// When the task row is already gone at the first write — a task stopped and
// then deleted while its last run was still finishing, since Stop marks it
// cancelled before the run has recorded its iteration — the row takes the
// runner's copy of the attribution instead (it.CreatedBy, CreatedByKeyID,
// Model), so that run's spend still counts for its creator, key and model.
// A present task row always wins over the copy, so the owner reassignment
// below behaves the same either way.
//
// The task row is read FOR SHARE: an owner reassignment holds it locked from
// before it rewrites the task until it commits, so a first write that lands
// meanwhile waits and copies the new owner, and one that got there first is
// committed before the reassignment's UPDATE of task_iterations runs, which
// then rewrites it. Without the lock a write in between would keep the owner
// being replaced. The runner holds no transaction on the task row while it
// records an iteration, so the wait is only ever on another writer's short
// transaction.
func (db *Database) AddTaskIteration(ctx context.Context, it *models.TaskIteration) error {
	if it.ID == uuid.Nil {
		it.ID = uuid.New()
	}
	_, err := db.conn.ExecContext(ctx, `
		WITH task AS (
			SELECT created_by, created_by_key_id, model FROM tasks WHERE id = $2 FOR SHARE
		)
		INSERT INTO task_iterations (
			id, task_id, iteration_number, started_at, completed_at, worker_session_id,
			exit_condition_result, cost_usd, prompt_tokens, completion_tokens, status,
			created_by, created_by_key_id, model
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
			CASE WHEN EXISTS (SELECT 1 FROM task) THEN (SELECT created_by FROM task) ELSE $12::uuid END,
			CASE WHEN EXISTS (SELECT 1 FROM task) THEN (SELECT created_by_key_id FROM task) ELSE $13::text END,
			CASE WHEN EXISTS (SELECT 1 FROM task) THEN (SELECT model FROM task) ELSE $14::text END)
		ON CONFLICT (id) DO UPDATE SET
			completed_at = EXCLUDED.completed_at,
			worker_session_id = EXCLUDED.worker_session_id,
			exit_condition_result = EXCLUDED.exit_condition_result,
			cost_usd = EXCLUDED.cost_usd,
			prompt_tokens = EXCLUDED.prompt_tokens,
			completion_tokens = EXCLUDED.completion_tokens,
			status = EXCLUDED.status`,
		it.ID,
		it.TaskID,
		it.IterationNumber,
		it.StartedAt,
		it.CompletedAt,
		nullableString(it.WorkerSessionID),
		nullableString(it.ExitConditionResult),
		it.CostUSD,
		it.PromptTokens,
		it.CompletionTokens,
		it.Status,
		it.CreatedBy,
		it.CreatedByKeyID,
		it.Model,
	)
	return err
}

// ListTaskIterations returns a task's iterations in iteration_number order.
func (db *Database) ListTaskIterations(ctx context.Context, taskID uuid.UUID) ([]*models.TaskIteration, error) {
	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+taskIterationColumns+" FROM task_iterations WHERE task_id = $1 ORDER BY iteration_number", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*models.TaskIteration
	for rows.Next() {
		it, serr := scanTaskIteration(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func scanTaskIteration(scanner interface{ Scan(...interface{}) error }) (*models.TaskIteration, error) {
	var (
		it                  models.TaskIteration
		completedAt         sql.NullTime
		workerSessionID     sql.NullString
		exitConditionResult sql.NullString
		costUSD             sql.NullFloat64
		promptTokens        sql.NullInt64
		completionTokens    sql.NullInt64
	)
	if err := scanner.Scan(
		&it.ID, &it.TaskID, &it.IterationNumber, &it.StartedAt, &completedAt,
		&workerSessionID, &exitConditionResult, &costUSD, &promptTokens, &completionTokens, &it.Status,
	); err != nil {
		return nil, err
	}
	if completedAt.Valid {
		t := completedAt.Time
		it.CompletedAt = &t
	}
	it.WorkerSessionID = workerSessionID.String
	it.ExitConditionResult = exitConditionResult.String
	it.CostUSD = costUSD.Float64
	it.PromptTokens = promptTokens.Int64
	it.CompletionTokens = completionTokens.Int64
	return &it, nil
}
