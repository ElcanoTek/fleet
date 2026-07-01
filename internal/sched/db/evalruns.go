package db

import (
	"context"
	"encoding/json"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// eval_runs accessors (#502). One row per `fleet eval run <set>` invocation:
// the set-level aggregate plus the marshaled per-case results. Rows are
// immutable once written — a re-run is a NEW row, so the per-set history stays
// an append-only regression record.

// evalRunColumns is the shared column list for eval_runs scans.
const evalRunColumns = "id, eval_set, started_at, completed_at, bundle_sha, total, passed, mean_score, threshold, pass, cost_usd, results"

func scanEvalRun(scanner interface{ Scan(...interface{}) error }) (*models.EvalRun, error) {
	var (
		r       models.EvalRun
		results []byte
	)
	if err := scanner.Scan(&r.ID, &r.EvalSet, &r.StartedAt, &r.CompletedAt, &r.BundleSHA,
		&r.Total, &r.Passed, &r.MeanScore, &r.Threshold, &r.Pass, &r.CostUSD, &results); err != nil {
		return nil, err
	}
	r.Results = json.RawMessage(results)
	return &r, nil
}

// AddEvalRun inserts one immutable eval-run record.
func (db *Database) AddEvalRun(ctx context.Context, r *models.EvalRun) error {
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO eval_runs (id, eval_set, started_at, completed_at, bundle_sha, total, passed, mean_score, threshold, pass, cost_usd, results)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		r.ID, r.EvalSet, r.StartedAt, r.CompletedAt, r.BundleSHA,
		r.Total, r.Passed, r.MeanScore, r.Threshold, r.Pass, r.CostUSD, string(r.Results))
	return err
}

// ListEvalRuns returns the newest-first run history for a set (every set when
// evalSet is empty), capped at limit (default 20 when <= 0).
func (db *Database) ListEvalRuns(ctx context.Context, evalSet string, limit int) ([]*models.EvalRun, error) {
	if limit <= 0 {
		limit = 20
	}
	// Sentinel-guarded filter (the ListFiltered pattern): no SQL concatenation.
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+evalRunColumns+` FROM eval_runs
		WHERE ($1 = '' OR eval_set = $1)
		ORDER BY started_at DESC
		LIMIT $2`, evalSet, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*models.EvalRun
	for rows.Next() {
		r, err := scanEvalRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestEvalRun returns the most recent run for a set, or nil when the set has
// never run — the baseline a new run's report compares against.
func (db *Database) LatestEvalRun(ctx context.Context, evalSet string) (*models.EvalRun, error) {
	runs, err := db.ListEvalRuns(ctx, evalSet, 1)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return runs[0], nil
}
