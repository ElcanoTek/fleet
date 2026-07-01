package db

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func testDataset() *models.Dataset {
	return &models.Dataset{
		ID:   uuid.New(),
		Name: "leads",
		Goal: "Research each company",
		Columns: []models.DatasetColumn{
			{Name: "company", Type: models.DatasetColumnText},
			{Name: "summary", Type: models.DatasetColumnText, Output: true},
		},
		Model:       "openrouter/auto",
		Status:      models.DatasetStatusIdle,
		Concurrency: 2,
	}
}

// setupDatasetFixture cleans the tables and creates one dataset with three rows.
func setupDatasetFixture(t *testing.T) (*Database, *models.Dataset) {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	for _, q := range []string{"DELETE FROM dataset_rows", "DELETE FROM datasets"} {
		if _, err := db.conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("clean: %v", err)
		}
	}
	d := testDataset()
	if err := db.CreateDataset(ctx, d); err != nil {
		t.Fatalf("CreateDataset: %v", err)
	}
	mk := func(company string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"company": company})
		return raw
	}
	if n, err := db.AddDatasetRows(ctx, d.ID, []json.RawMessage{mk("a"), mk("b")}); err != nil || n != 2 {
		t.Fatalf("AddDatasetRows: %d %v", n, err)
	}
	if n, err := db.AddDatasetRows(ctx, d.ID, []json.RawMessage{mk("c")}); err != nil || n != 1 {
		t.Fatalf("append: %d %v", n, err)
	}
	return db, d
}

func TestDatasetCRUDAndImport(t *testing.T) {
	db, d := setupDatasetFixture(t)
	ctx := context.Background()

	got, err := db.GetDataset(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDataset: %v", err)
	}
	if got.Name != d.Name || len(got.Columns) != 2 || !got.Columns[1].Output || got.Concurrency != 2 {
		t.Fatalf("round-trip: %+v", got)
	}
	rows, err := db.ListDatasetRows(ctx, d.ID, "", 0, 0)
	if err != nil || len(rows) != 3 {
		t.Fatalf("ListDatasetRows: %d %v", len(rows), err)
	}
	if rows[2].RowIndex != 2 {
		t.Fatalf("append must continue indexes: %+v", rows[2])
	}

	list, err := db.ListDatasets(ctx)
	if err != nil || len(list) != 1 || list[0].RowCounts[models.DatasetRowPending] != 3 {
		t.Fatalf("ListDatasets: %v %v", list, err)
	}

	if err := db.DeleteDataset(ctx, d.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, _ = db.ListDatasetRows(ctx, d.ID, "", 0, 0)
	if len(rows) != 0 {
		t.Fatalf("cascade failed: %d rows", len(rows))
	}
}

func TestDatasetClaimAndOutcomes(t *testing.T) {
	db, d := setupDatasetFixture(t)
	ctx := context.Background()

	r1, err := db.ClaimNextDatasetRow(ctx, d.ID)
	if err != nil || r1 == nil || r1.RowIndex != 0 || r1.Attempts != 1 {
		t.Fatalf("claim 1: %+v %v", r1, err)
	}
	r2, err := db.ClaimNextDatasetRow(ctx, d.ID)
	if err != nil || r2 == nil || r2.RowIndex != 1 {
		t.Fatalf("claim 2: %+v %v", r2, err)
	}

	if err := db.FinishDatasetRow(ctx, r1.ID, json.RawMessage(`{"summary":"fine"}`), "", "", 0.02); err != nil {
		t.Fatalf("finish proposed: %v", err)
	}
	if err := db.FinishDatasetRow(ctx, r2.ID, nil, "free-form essay", "did not conform", 0.01); err != nil {
		t.Fatalf("finish failed: %v", err)
	}
	// A late write against a non-running row is rejected (reset/approve win).
	if err := db.FinishDatasetRow(ctx, r1.ID, nil, "", "late", 0); err == nil {
		t.Fatal("late finish must be rejected")
	}

	proposed, err := db.ListDatasetRows(ctx, d.ID, models.DatasetRowProposed, 0, 0)
	if err != nil || len(proposed) != 1 || string(proposed[0].Proposed) != `{"summary": "fine"}` && string(proposed[0].Proposed) != `{"summary":"fine"}` {
		t.Fatalf("proposed rows: %+v %v", proposed, err)
	}
	failed, _ := db.ListDatasetRows(ctx, d.ID, models.DatasetRowFailed, 0, 0)
	if len(failed) != 1 || failed[0].ResultNote != "free-form essay" || failed[0].CostUSD != 0.01 {
		t.Fatalf("failed rows: %+v", failed)
	}
}

func TestDatasetReviewResetAndSweep(t *testing.T) {
	db, d := setupDatasetFixture(t)
	ctx := context.Background()

	r1, _ := db.ClaimNextDatasetRow(ctx, d.ID)
	r2, _ := db.ClaimNextDatasetRow(ctx, d.ID)
	if err := db.FinishDatasetRow(ctx, r1.ID, json.RawMessage(`{"summary":"fine"}`), "", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishDatasetRow(ctx, r2.ID, nil, "essay", "did not conform", 0); err != nil {
		t.Fatal(err)
	}

	// Approve merges proposed into cells (JSONB ||) and clears the proposal.
	if n, err := db.ApproveDatasetRows(ctx, d.ID, nil); err != nil || n != 1 {
		t.Fatalf("approve: %d %v", n, err)
	}
	approved, _ := db.ListDatasetRows(ctx, d.ID, models.DatasetRowApproved, 0, 0)
	if len(approved) != 1 {
		t.Fatalf("approved rows: %d", len(approved))
	}
	var cells map[string]any
	_ = json.Unmarshal(approved[0].Cells, &cells)
	if cells["summary"] != "fine" || cells["company"] != "a" {
		t.Fatalf("approve must merge output into cells: %v", cells)
	}
	if len(approved[0].Proposed) != 0 {
		t.Fatal("approve must clear proposed")
	}

	// Bulk retry resets only failed rows when ids are empty.
	if n, err := db.ResetDatasetRows(ctx, d.ID, nil); err != nil || n != 1 {
		t.Fatalf("bulk reset: %d %v", n, err)
	}
	counts, _ := db.datasetRowCounts(ctx, d.ID)
	if counts[models.DatasetRowPending] != 2 || counts[models.DatasetRowApproved] != 1 {
		t.Fatalf("counts after reset: %v", counts)
	}

	// Guarded status transitions.
	if ok, _ := db.UpdateDatasetStatus(ctx, d.ID, []string{models.DatasetStatusIdle, models.DatasetStatusPaused}, models.DatasetStatusRunning); !ok {
		t.Fatal("idle→running should apply")
	}
	if ok, _ := db.UpdateDatasetStatus(ctx, d.ID, []string{models.DatasetStatusIdle}, models.DatasetStatusRunning); ok {
		t.Fatal("running→running must not apply")
	}

	// Boot sweep: running dataset + running rows → paused/pending.
	if _, err := db.ClaimNextDatasetRow(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ResetStaleRunningDatasets(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, _ := db.GetDataset(ctx, d.ID)
	if got.Status != models.DatasetStatusPaused || got.RowCounts[models.DatasetRowRunning] != 0 {
		t.Fatalf("sweep: status=%s counts=%v", got.Status, got.RowCounts)
	}
}
