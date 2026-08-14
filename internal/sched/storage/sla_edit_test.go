package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestTaskSLAEditRoundTrip pins the SLA columns through the edit path (#274):
// an edit can set, change, and clear a task's expected duration (nil = no SLA,
// so the monitor skips it), and the multiplier thresholds resolve non-positive
// payload values to the defaults — the same rule NewTask applies — so the
// NOT NULL columns never persist an unusable threshold.
func TestTaskSLAEditRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	task := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Set an SLA with explicit thresholds.
	expected := 45
	edit := TaskEdit{Prompt: "p", ExpectedDurationMinutes: &expected, SLAWarnMultiplier: 1.2, SLAFailMultiplier: 3}
	upd, err := store.UpdateEditableTask(ctx, task.ID, edit)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if upd.ExpectedDurationMinutes == nil || *upd.ExpectedDurationMinutes != 45 {
		t.Errorf("edit should have set expected duration, got %v", upd.ExpectedDurationMinutes)
	}
	if upd.SLAWarnMultiplier != 1.2 || upd.SLAFailMultiplier != 3 {
		t.Errorf("edit should have set multipliers, got %v/%v", upd.SLAWarnMultiplier, upd.SLAFailMultiplier)
	}

	// Omitting the multipliers (zero values) resolves them back to the defaults.
	expected = 60
	upd, err = store.UpdateEditableTask(ctx, task.ID, TaskEdit{Prompt: "p", ExpectedDurationMinutes: &expected})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if upd.ExpectedDurationMinutes == nil || *upd.ExpectedDurationMinutes != 60 {
		t.Errorf("edit should have changed expected duration, got %v", upd.ExpectedDurationMinutes)
	}
	if upd.SLAWarnMultiplier != models.DefaultSLAWarnMultiplier || upd.SLAFailMultiplier != models.DefaultSLAFailMultiplier {
		t.Errorf("omitted multipliers should resolve to defaults, got %v/%v", upd.SLAWarnMultiplier, upd.SLAFailMultiplier)
	}

	// Omitting the expected duration clears the SLA entirely.
	upd, err = store.UpdateEditableTask(ctx, task.ID, TaskEdit{Prompt: "p"})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if upd.ExpectedDurationMinutes != nil {
		t.Errorf("edit should have cleared the SLA, got %v", *upd.ExpectedDurationMinutes)
	}

	// The write must survive a fresh read, not just the returned struct.
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExpectedDurationMinutes != nil {
		t.Errorf("cleared SLA should round-trip nil, got %v", *got.ExpectedDurationMinutes)
	}
}
