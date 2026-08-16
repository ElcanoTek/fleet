// Per-principal rolling budgets (#601 part 2): persistence for the budgets
// table (migration 052). A budget row holds only CONFIGURATION plus the
// soft-alert dedup marker — spend is never accumulated here. Enforcement
// recomputes the current-window spend from the part-1 usage read model
// (TaskUsage in usage.go + the chat store's turn_metrics seam), so this table
// can never drift into a second accounting path.

package db

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// budgetColumns is the shared column list for budgets scans.
const budgetColumns = "id, scope, principal_id, time_window, soft_usd, hard_usd, " +
	"soft_tokens, hard_tokens, soft_alert_window_start, soft_alert_at, created_at, updated_at"

func scanBudget(scanner interface{ Scan(...interface{}) error }) (*models.Budget, error) {
	var (
		b           models.Budget
		softUSD     sql.NullFloat64
		hardUSD     sql.NullFloat64
		softTokens  sql.NullInt64
		hardTokens  sql.NullInt64
		alertWindow sql.NullTime
		alertAt     sql.NullTime
	)
	if err := scanner.Scan(&b.ID, &b.Scope, &b.PrincipalID, &b.Window, &softUSD, &hardUSD,
		&softTokens, &hardTokens, &alertWindow, &alertAt, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return nil, err
	}
	if softUSD.Valid {
		b.SoftUSD = &softUSD.Float64
	}
	if hardUSD.Valid {
		b.HardUSD = &hardUSD.Float64
	}
	if softTokens.Valid {
		b.SoftTokens = &softTokens.Int64
	}
	if hardTokens.Valid {
		b.HardTokens = &hardTokens.Int64
	}
	if alertWindow.Valid {
		t := alertWindow.Time.UTC()
		b.SoftAlertWindowStart = &t
	}
	if alertAt.Valid {
		t := alertAt.Time.UTC()
		b.SoftAlertAt = &t
	}
	return &b, nil
}

// UpsertBudget inserts or replaces the budget for (scope, principal, window)
// and returns the persisted row. An upsert RESETS the soft-alert marker:
// editing a budget's thresholds re-arms its once-per-window alert, otherwise a
// raised soft bound could never alert again within the window it was raised in.
func (db *Database) UpsertBudget(ctx context.Context, bc models.BudgetCreate) (*models.Budget, error) {
	row := db.conn.QueryRowContext(ctx, `
		INSERT INTO budgets (scope, principal_id, time_window, soft_usd, hard_usd, soft_tokens, hard_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (scope, principal_id, time_window) DO UPDATE SET
			soft_usd = EXCLUDED.soft_usd,
			hard_usd = EXCLUDED.hard_usd,
			soft_tokens = EXCLUDED.soft_tokens,
			hard_tokens = EXCLUDED.hard_tokens,
			soft_alert_window_start = NULL,
			soft_alert_at = NULL,
			updated_at = now()
		RETURNING `+budgetColumns,
		bc.Scope, bc.PrincipalID, bc.Window, bc.SoftUSD, bc.HardUSD, bc.SoftTokens, bc.HardTokens)
	return scanBudget(row)
}

// ListBudgets returns every configured budget, ordered for stable rendering.
func (db *Database) ListBudgets(ctx context.Context) ([]models.Budget, error) {
	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+budgetColumns+" FROM budgets ORDER BY scope, principal_id, time_window")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Budget
	for rows.Next() {
		b, err := scanBudget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// BudgetsFor returns every budget matching the principals one create carries:
// user (sched username), key (scoped API key id).
// Empty arguments never match — the predicate requires a non-empty parameter
// per scope, so a create path lacking a principal can't accidentally match a
// budget whose principal_id is empty.
func (db *Database) BudgetsFor(ctx context.Context, user, key string) ([]models.Budget, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+budgetColumns+` FROM budgets
		WHERE (scope = 'user'    AND $1 <> '' AND principal_id = $1)
		   OR (scope = 'key'     AND $2 <> '' AND principal_id = $2)
		ORDER BY scope, time_window`,
		user, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Budget
	for rows.Next() {
		b, err := scanBudget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// DeleteBudget removes a budget by id. ok=false when no row matched.
func (db *Database) DeleteBudget(ctx context.Context, id uuid.UUID) (bool, error) {
	res, err := db.conn.ExecContext(ctx, "DELETE FROM budgets WHERE id = $1", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkBudgetSoftAlert claims the one-per-window soft alert for the window
// starting at windowStart. The UPDATE is conditional on the persisted marker
// differing from windowStart, so of N concurrent creates that all observe the
// crossing exactly one wins the claim (fired=true) and fires the notification;
// the marker persisting in the DB is what makes a restart unable to re-alert.
func (db *Database) MarkBudgetSoftAlert(ctx context.Context, id uuid.UUID, windowStart time.Time) (bool, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE budgets SET soft_alert_window_start = $2, soft_alert_at = now()
		WHERE id = $1 AND (soft_alert_window_start IS NULL OR soft_alert_window_start <> $2)`,
		id, windowStart.UTC())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
