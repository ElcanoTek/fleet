package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/truncate"
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

// TestRequeueDatasetRow pins the #586 pause contract at the DB layer: a
// running row returns to pending with the claim's attempt refunded, is
// re-claimable, and a non-running row is rejected (reset/approve win).
func TestRequeueDatasetRow(t *testing.T) {
	db, d := setupDatasetFixture(t)
	ctx := context.Background()

	r1, err := db.ClaimNextDatasetRow(ctx, d.ID)
	if err != nil || r1 == nil || r1.Attempts != 1 {
		t.Fatalf("claim: %+v %v", r1, err)
	}
	if err := db.RequeueDatasetRow(ctx, r1.ID); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	pending, err := db.ListDatasetRows(ctx, d.ID, models.DatasetRowPending, 0, 0)
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending after requeue: %d %v", len(pending), err)
	}
	for _, r := range pending {
		if r.ID == r1.ID && r.Attempts != 0 {
			t.Fatalf("requeued row attempts = %d, want 0 (refunded)", r.Attempts)
		}
	}
	// The requeued row is claimable again (lowest row_index first).
	again, err := db.ClaimNextDatasetRow(ctx, d.ID)
	if err != nil || again == nil || again.ID != r1.ID || again.Attempts != 1 {
		t.Fatalf("reclaim: %+v %v", again, err)
	}
	// A row that already reached an outcome must not be requeued.
	if err := db.FinishDatasetRow(ctx, again.ID, json.RawMessage(`{"summary":"fine"}`), "", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := db.RequeueDatasetRow(ctx, again.ID); err == nil {
		t.Fatal("requeue of a non-running row must be rejected")
	}
}

// TestFinishDatasetRow_ClampedMultibyteTextLands pins the #595 datasets fix:
// note/error text clamped mid-multibyte-rune used to reach Postgres as invalid
// UTF-8, the TEXT write was rejected, and the row stranded 'running'. The
// rune-boundary clamp keeps the write valid so the outcome always lands.
func TestFinishDatasetRow_ClampedMultibyteTextLands(t *testing.T) {
	db, d := setupDatasetFixture(t)
	ctx := context.Background()

	r1, err := db.ClaimNextDatasetRow(ctx, d.ID)
	if err != nil || r1 == nil {
		t.Fatalf("claim: %+v %v", r1, err)
	}
	// 3-byte runes with a budget that is not a multiple of 3: the naive byte
	// slice at the budget is invalid UTF-8 (the pre-fix behavior).
	const budget = 8000
	long := strings.Repeat("世", budget/3+2)
	if utf8.ValidString(long[:budget]) {
		t.Fatal("test bug: naive byte slice is valid UTF-8 — not exercising the regression")
	}
	note := truncate.Clamp(long, budget, "…[truncated]")
	errMsg := truncate.Clamp(long, 500, "…[truncated]")
	if !utf8.ValidString(note) || !utf8.ValidString(errMsg) {
		t.Fatal("clamped text must be valid UTF-8")
	}
	if err := db.FinishDatasetRow(ctx, r1.ID, nil, note, errMsg, 0); err != nil {
		t.Fatalf("FinishDatasetRow with clamped multibyte text must land: %v", err)
	}
	failed, err := db.ListDatasetRows(ctx, d.ID, models.DatasetRowFailed, 0, 0)
	if err != nil || len(failed) != 1 || failed[0].ID != r1.ID {
		t.Fatalf("failed rows: %d %v", len(failed), err)
	}
	if failed[0].ResultNote != note || failed[0].Error != errMsg {
		t.Fatal("clamped note/error must round-trip unchanged")
	}
}

// TestAddDatasetRowsRejectsOversizedBatch verifies the defensive count bound is
// enforced before the args slice is sized (go/allocation-size-overflow) — and
// before any DB access, so the check holds without a live database.
func TestAddDatasetRowsRejectsOversizedBatch(t *testing.T) {
	cells := make([]json.RawMessage, maxDatasetRowsPerInsert+1)
	for i := range cells {
		cells[i] = json.RawMessage(`{}`)
	}
	// nil conn is never dereferenced: the bound check returns before BeginTx.
	db := &Database{}
	n, err := db.AddDatasetRows(context.Background(), uuid.New(), cells)
	if err == nil {
		t.Fatal("oversized batch must be rejected")
	}
	if n != 0 {
		t.Fatalf("rejected batch must report 0 rows, got %d", n)
	}
	if !strings.Contains(err.Error(), "too many rows") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestListDatasetsRowCountsPerDataset pins the batched row-count query in
// ListDatasets to per-dataset attribution. The counts used to come from one
// query per dataset, which could not mix them up; the grouped form can, and the
// single-dataset fixture above would not notice. A dataset with no rows must
// still come back with an empty (not nil) map, as the per-dataset form did.
func TestListDatasetsRowCountsPerDataset(t *testing.T) {
	db, first := setupDatasetFixture(t)
	ctx := context.Background()

	mk := func(company string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"company": company})
		return raw
	}

	second := testDataset()
	second.Name = "vendors"
	if err := db.CreateDataset(ctx, second); err != nil {
		t.Fatalf("CreateDataset second: %v", err)
	}
	if n, err := db.AddDatasetRows(ctx, second.ID, []json.RawMessage{mk("x")}); err != nil || n != 1 {
		t.Fatalf("AddDatasetRows second: %d %v", n, err)
	}

	// A third dataset with zero rows: absent from the grouped result entirely.
	empty := testDataset()
	empty.Name = "empty"
	if err := db.CreateDataset(ctx, empty); err != nil {
		t.Fatalf("CreateDataset empty: %v", err)
	}

	// Move one of the first dataset's rows off pending so the two datasets
	// differ in both count and status set.
	claimed, err := db.ClaimNextDatasetRow(ctx, first.ID)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %+v %v", claimed, err)
	}

	list, err := db.ListDatasets(ctx)
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 datasets, got %d", len(list))
	}
	byID := map[uuid.UUID]*models.Dataset{}
	for _, d := range list {
		byID[d.ID] = d
	}

	if got := byID[first.ID].RowCounts[models.DatasetRowPending]; got != 2 {
		t.Fatalf("first pending: want 2, got %d (%v)", got, byID[first.ID].RowCounts)
	}
	if got := byID[first.ID].RowCounts[models.DatasetRowRunning]; got != 1 {
		t.Fatalf("first running: want 1, got %d (%v)", got, byID[first.ID].RowCounts)
	}
	if got := byID[second.ID].RowCounts[models.DatasetRowPending]; got != 1 {
		t.Fatalf("second pending: want 1, got %d (%v)", got, byID[second.ID].RowCounts)
	}
	if got := byID[second.ID].RowCounts[models.DatasetRowRunning]; got != 0 {
		t.Fatalf("second must not inherit the first dataset's statuses: %v", byID[second.ID].RowCounts)
	}
	if counts := byID[empty.ID].RowCounts; counts == nil || len(counts) != 0 {
		t.Fatalf("row-less dataset wants an empty non-nil map, got %#v", counts)
	}
}
