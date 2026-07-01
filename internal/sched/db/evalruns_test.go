package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestEvalRunsRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, "DELETE FROM eval_runs"); err != nil {
		t.Fatalf("clean eval_runs: %v", err)
	}

	base := time.Now().UTC().Truncate(time.Second)
	mk := func(set string, started time.Time, pass bool, mean float64) *models.EvalRun {
		return &models.EvalRun{
			ID:          uuid.New(),
			EvalSet:     set,
			StartedAt:   started,
			CompletedAt: started.Add(time.Minute),
			BundleSHA:   "sha256:abc",
			Total:       3,
			Passed:      2,
			MeanScore:   mean,
			Threshold:   0.5,
			Pass:        pass,
			CostUSD:     0.12,
			Results:     json.RawMessage(`[{"name":"c1","pass":true}]`),
		}
	}

	old := mk("smoke", base.Add(-2*time.Hour), false, 0.4)
	newer := mk("smoke", base.Add(-1*time.Hour), true, 0.9)
	other := mk("other", base, true, 1.0)
	for _, r := range []*models.EvalRun{old, newer, other} {
		if err := db.AddEvalRun(ctx, r); err != nil {
			t.Fatalf("AddEvalRun: %v", err)
		}
	}

	runs, err := db.ListEvalRuns(ctx, "smoke", 10)
	if err != nil {
		t.Fatalf("ListEvalRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 smoke runs, got %d", len(runs))
	}
	if runs[0].ID != newer.ID || runs[1].ID != old.ID {
		t.Fatal("runs must list newest first")
	}
	got := runs[0]
	if got.EvalSet != "smoke" || !got.Pass || got.MeanScore != 0.9 || got.Threshold != 0.5 ||
		got.Total != 3 || got.Passed != 2 || got.BundleSHA != "sha256:abc" || got.CostUSD != 0.12 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.StartedAt.Equal(newer.StartedAt) || !got.CompletedAt.Equal(newer.CompletedAt) {
		t.Fatalf("timestamps: got %v / %v", got.StartedAt, got.CompletedAt)
	}
	var cases []map[string]any
	if err := json.Unmarshal(got.Results, &cases); err != nil || len(cases) != 1 || cases[0]["name"] != "c1" {
		t.Fatalf("results JSONB round-trip: %s (%v)", got.Results, err)
	}

	// Empty set filter = all sets.
	all, err := db.ListEvalRuns(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 total runs, got %d", len(all))
	}

	latest, err := db.LatestEvalRun(ctx, "smoke")
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.ID != newer.ID {
		t.Fatalf("LatestEvalRun: %+v", latest)
	}
	none, err := db.LatestEvalRun(ctx, "never-ran")
	if err != nil {
		t.Fatal(err)
	}
	if none != nil {
		t.Fatalf("never-ran set must return nil, got %+v", none)
	}
}
