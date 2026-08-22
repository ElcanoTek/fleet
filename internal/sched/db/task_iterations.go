package db

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// ── task_iterations (looped-task telemetry, #179) ──

const taskIterationColumns = "id, task_id, iteration_number, started_at, completed_at, worker_session_id, exit_condition_result, cost_usd, prompt_tokens, completion_tokens, status"

// AddTaskIteration inserts or updates a per-iteration telemetry row (upsert on
// id, so a row created at iteration start can be finalized at iteration end).
func (db *Database) AddTaskIteration(ctx context.Context, it *models.TaskIteration) error {
	if it.ID == uuid.Nil {
		it.ID = uuid.New()
	}
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO task_iterations (
			id, task_id, iteration_number, started_at, completed_at, worker_session_id,
			exit_condition_result, cost_usd, prompt_tokens, completion_tokens, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
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
