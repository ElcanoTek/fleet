package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// datasets / dataset_rows accessors (#514). Row write-backs are PROPOSALS:
// the runner sets status='proposed' with the validated JSON; only the
// human-driven ApproveDatasetRows merges proposed values into cells.

const datasetColumns = "id, name, goal, columns, model, persona, status, concurrency, created_at, updated_at"

func scanDataset(scanner interface{ Scan(...interface{}) error }) (*models.Dataset, error) {
	var (
		d    models.Dataset
		cols []byte
	)
	if err := scanner.Scan(&d.ID, &d.Name, &d.Goal, &cols, &d.Model, &d.Persona,
		&d.Status, &d.Concurrency, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(cols, &d.Columns); err != nil {
		return nil, fmt.Errorf("dataset %s: decode columns: %w", d.ID, err)
	}
	return &d, nil
}

const datasetRowColumns = "id, dataset_id, row_index, cells, status, proposed, result_note, error, attempts, cost_usd, updated_at"

func scanDatasetRow(scanner interface{ Scan(...interface{}) error }) (*models.DatasetRow, error) {
	var (
		r        models.DatasetRow
		cells    []byte
		proposed sql.NullString
	)
	if err := scanner.Scan(&r.ID, &r.DatasetID, &r.RowIndex, &cells, &r.Status, &proposed,
		&r.ResultNote, &r.Error, &r.Attempts, &r.CostUSD, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.Cells = json.RawMessage(cells)
	if proposed.Valid && proposed.String != "" {
		r.Proposed = json.RawMessage(proposed.String)
	}
	return &r, nil
}

// CreateDataset inserts the definition. Columns are marshaled verbatim — the
// handler validates them (names, types, ≥1 output column) before this.
func (db *Database) CreateDataset(ctx context.Context, d *models.Dataset) error {
	cols, err := json.Marshal(d.Columns)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `
		INSERT INTO datasets (id, name, goal, columns, model, persona, status, concurrency)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		d.ID, d.Name, d.Goal, string(cols), d.Model, d.Persona, d.Status, d.Concurrency)
	return err
}

// GetDataset returns one dataset with its per-status row counts.
func (db *Database) GetDataset(ctx context.Context, id uuid.UUID) (*models.Dataset, error) {
	row := db.conn.QueryRowContext(ctx, "SELECT "+datasetColumns+" FROM datasets WHERE id = $1", id)
	d, err := scanDataset(row)
	if err != nil {
		return nil, err
	}
	counts, err := db.datasetRowCounts(ctx, id)
	if err != nil {
		return nil, err
	}
	d.RowCounts = counts
	return d, nil
}

func (db *Database) datasetRowCounts(ctx context.Context, id uuid.UUID) (map[string]int, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM dataset_rows WHERE dataset_id = $1 GROUP BY status`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		counts[status] = n
	}
	return counts, rows.Err()
}

// ListDatasets returns every dataset, newest first, with row counts.
func (db *Database) ListDatasets(ctx context.Context) ([]*models.Dataset, error) {
	rows, err := db.conn.QueryContext(ctx, "SELECT "+datasetColumns+" FROM datasets ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*models.Dataset
	for rows.Next() {
		d, err := scanDataset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range out {
		counts, err := db.datasetRowCounts(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		d.RowCounts = counts
	}
	return out, nil
}

// UpdateDatasetStatus moves the table-level run state (idle|running|paused).
// The guarded form prevents two concurrent Run calls from both claiming: the
// transition only applies from the expected prior status; returns whether it
// applied.
func (db *Database) UpdateDatasetStatus(ctx context.Context, id uuid.UUID, from []string, to string) (bool, error) {
	res, err := db.conn.ExecContext(ctx,
		`UPDATE datasets SET status = $1, updated_at = now() WHERE id = $2 AND status = ANY($3)`,
		to, id, pqStringArray(from))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteDataset removes the dataset and (via FK cascade) its rows.
func (db *Database) DeleteDataset(ctx context.Context, id uuid.UUID) error {
	res, err := db.conn.ExecContext(ctx, `DELETE FROM datasets WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AddDatasetRows bulk-inserts rows starting at the dataset's next row_index,
// in one statement per batch (the AddTaskBatch pattern). Returns rows added.
func (db *Database) AddDatasetRows(ctx context.Context, datasetID uuid.UUID, cells []json.RawMessage) (int, error) {
	if len(cells) == 0 {
		return 0, nil
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(row_index), -1) + 1 FROM dataset_rows WHERE dataset_id = $1`, datasetID,
	).Scan(&next); err != nil {
		return 0, err
	}

	// Single parameterized multi-row insert (the AddTaskBatch pattern): only
	// "$N" placeholders are assembled; every value rides args.
	var q strings.Builder
	q.WriteString("INSERT INTO dataset_rows (id, dataset_id, row_index, cells) VALUES ")
	args := make([]interface{}, 0, len(cells)*4)
	for i, c := range cells {
		if i > 0 {
			q.WriteString(", ")
		}
		base := i * 4
		fmt.Fprintf(&q, "($%d, $%d, $%d, $%d)", base+1, base+2, base+3, base+4)
		args = append(args, uuid.New(), datasetID, next+i, string(c))
	}
	if _, err := tx.ExecContext(ctx, q.String(), args...); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(cells), nil
}

// ListDatasetRows returns rows ordered by row_index, optionally filtered by
// status, with limit/offset paging (limit <= 0 → 200).
func (db *Database) ListDatasetRows(ctx context.Context, datasetID uuid.UUID, status string, limit, offset int) ([]*models.DatasetRow, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+datasetRowColumns+` FROM dataset_rows
		WHERE dataset_id = $1 AND ($2 = '' OR status = $2)
		ORDER BY row_index ASC LIMIT $3 OFFSET $4`,
		datasetID, status, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*models.DatasetRow
	for rows.Next() {
		r, err := scanDatasetRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimNextDatasetRow atomically claims ONE pending row (pending→running,
// attempts+1) and returns it, or nil when none remain. The runner's worker
// goroutines each loop on this, so concurrent workers can never claim the
// same row.
func (db *Database) ClaimNextDatasetRow(ctx context.Context, datasetID uuid.UUID) (*models.DatasetRow, error) {
	row := db.conn.QueryRowContext(ctx, `
		UPDATE dataset_rows SET status = 'running', attempts = attempts + 1, updated_at = now()
		WHERE id = (
			SELECT id FROM dataset_rows
			WHERE dataset_id = $1 AND status = 'pending'
			ORDER BY row_index ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+datasetRowColumns, datasetID)
	r, err := scanDatasetRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return r, nil
}

// FinishDatasetRow records one row run's outcome: proposed JSON (→ status
// 'proposed'), or a failure (→ 'failed' with the error). The note carries the
// free-form answer when the output didn't validate — stored for review, never
// written to cells. Guarded on status='running' so a concurrent reset/approve
// can't be clobbered by a late runner write.
func (db *Database) FinishDatasetRow(ctx context.Context, rowID uuid.UUID, proposed json.RawMessage, note, errMsg string, costUSD float64) error {
	var proposedArg interface{}
	status := models.DatasetRowFailed
	if len(proposed) > 0 {
		proposedArg = string(proposed)
		status = models.DatasetRowProposed
	}
	res, err := db.conn.ExecContext(ctx, `
		UPDATE dataset_rows SET status = $1, proposed = $2, result_note = $3, error = $4,
			cost_usd = cost_usd + $5, updated_at = now()
		WHERE id = $6 AND status = 'running'`,
		status, proposedArg, note, errMsg, costUSD, rowID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("dataset row %s no longer running (reset or approved mid-run)", rowID)
	}
	return nil
}

// ApproveDatasetRows merges each proposed object into cells for the given
// rows (every row when ids is empty), sets status 'approved', and clears
// proposed. JSONB || merges top-level keys — exactly the output-column
// write-back contract. Returns rows approved.
func (db *Database) ApproveDatasetRows(ctx context.Context, datasetID uuid.UUID, ids []uuid.UUID) (int, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE dataset_rows
		SET cells = cells || proposed, proposed = NULL, status = 'approved', error = '', updated_at = now()
		WHERE dataset_id = $1 AND status = 'proposed' AND proposed IS NOT NULL
		  AND (cardinality($2::uuid[]) = 0 OR id = ANY($2::uuid[]))`,
		datasetID, pqUUIDArray(ids))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ResetDatasetRows returns rows to 'pending' for a re-run: the given ids, or
// every failed row when ids is empty (the bulk retry). Proposed/error state
// is cleared; approved cells stay as-is (a re-run may propose new values over
// them). Running rows are never reset (the runner owns them).
func (db *Database) ResetDatasetRows(ctx context.Context, datasetID uuid.UUID, ids []uuid.UUID) (int, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE dataset_rows
		SET status = 'pending', proposed = NULL, result_note = '', error = '', updated_at = now()
		WHERE dataset_id = $1 AND status != 'running'
		  AND ((cardinality($2::uuid[]) = 0 AND status = 'failed') OR id = ANY($2::uuid[]))`,
		datasetID, pqUUIDArray(ids))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ResetStaleRunningDatasets is the boot sweep: a crash mid-run leaves
// datasets 'running' and rows 'running' with no owner. Reset both so work is
// resumable (rows go back to pending; the dataset parks as paused).
func (db *Database) ResetStaleRunningDatasets(ctx context.Context) error {
	if _, err := db.conn.ExecContext(ctx,
		`UPDATE dataset_rows SET status = 'pending', updated_at = now() WHERE status = 'running'`); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx,
		`UPDATE datasets SET status = 'paused', updated_at = now() WHERE status = 'running'`)
	return err
}

// pqStringArray / pqUUIDArray render Postgres array literals for ANY($n)
// parameters without importing lib/pq (the driver is pgx stdlib).
func pqStringArray(vals []string) string {
	out := "{"
	for i, v := range vals {
		if i > 0 {
			out += ","
		}
		out += `"` + v + `"`
	}
	return out + "}"
}

func pqUUIDArray(ids []uuid.UUID) string {
	out := "{"
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id.String()
	}
	return out + "}"
}
