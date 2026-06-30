package db

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestStructuredOutput_RoundTrip persists a task's output_schema and output_json
// JSONB columns and reads them back unchanged (#244); a task with neither
// round-trips as nil for both.
func TestStructuredOutput_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)
	withSchema := &models.Task{
		ID:           uuid.New(),
		Prompt:       "structured",
		Status:       models.TaskStatusPending,
		CreatedAt:    time.Now().UTC(),
		OutputSchema: schema,
	}
	none := &models.Task{
		ID:        uuid.New(),
		Prompt:    "free-form",
		Status:    models.TaskStatusPending,
		CreatedAt: time.Now().UTC(),
	}
	for _, tk := range []*models.Task{withSchema, none} {
		if err := db.AddTask(ctx, tk); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
	}

	got, err := db.GetTask(ctx, withSchema.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !jsonEqual(got.OutputSchema, schema) {
		t.Errorf("OutputSchema round-trip = %s, want %s", got.OutputSchema, schema)
	}
	if got.OutputJSON != nil {
		t.Errorf("OutputJSON should be nil before a result is recorded, got %s", got.OutputJSON)
	}

	gotNone, err := db.GetTask(ctx, none.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if gotNone.OutputSchema != nil || gotNone.OutputJSON != nil {
		t.Errorf("task with no structured output should have nil schema+json, got %s / %s", gotNone.OutputSchema, gotNone.OutputJSON)
	}

	// Recording the validated result (the runner's path) round-trips output_json.
	result := json.RawMessage(`{"n":42}`)
	got.OutputJSON = result
	if err := db.UpdateTask(ctx, got); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	reread, err := db.GetTask(ctx, withSchema.ID)
	if err != nil {
		t.Fatalf("GetTask after update: %v", err)
	}
	if !jsonEqual(reread.OutputJSON, result) {
		t.Errorf("OutputJSON round-trip = %s, want %s", reread.OutputJSON, result)
	}
	// The schema is unchanged by the result update.
	if !jsonEqual(reread.OutputSchema, schema) {
		t.Errorf("OutputSchema mutated by result update: %s", reread.OutputSchema)
	}
}

// jsonEqual compares two raw JSON values semantically (Postgres JSONB normalizes
// object key order + whitespace, so a byte compare would spuriously fail).
func jsonEqual(a, b json.RawMessage) bool {
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}
