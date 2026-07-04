package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestThinkingBudget_RoundTrip: the per-task thinking override (#220) persists
// through INSERT and the upsert-based UpdateTask, distinguishing nil (inherit),
// 0 (off), and a positive budget. Mirrors TestSandboxLimits_RoundTrip.
func TestThinkingBudget_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	ptr := func(i int) *int { return &i }
	set := &models.Task{ID: uuid.New(), Prompt: "budget", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), ThinkingBudgetTokens: ptr(8192)}
	off := &models.Task{ID: uuid.New(), Prompt: "off", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), ThinkingBudgetTokens: ptr(0)}
	inherit := &models.Task{ID: uuid.New(), Prompt: "inherit", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	for _, tk := range []*models.Task{set, off, inherit} {
		if err := db.AddTask(ctx, tk); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
	}

	gotSet, _ := db.GetTask(ctx, set.ID)
	if gotSet.ThinkingBudgetTokens == nil || *gotSet.ThinkingBudgetTokens != 8192 {
		t.Errorf("set budget round-trip = %v, want 8192", gotSet.ThinkingBudgetTokens)
	}
	gotOff, _ := db.GetTask(ctx, off.ID)
	if gotOff.ThinkingBudgetTokens == nil || *gotOff.ThinkingBudgetTokens != 0 {
		t.Errorf("explicit-off (0) must persist distinctly from nil, got %v", gotOff.ThinkingBudgetTokens)
	}
	gotInherit, _ := db.GetTask(ctx, inherit.ID)
	if gotInherit.ThinkingBudgetTokens != nil {
		t.Errorf("unset must round-trip as nil (inherit), got %v", gotInherit.ThinkingBudgetTokens)
	}

	// UpdateTask (upsert) preserves + can change the budget.
	v := 16384
	gotSet.ThinkingBudgetTokens = &v
	if err := db.UpdateTask(ctx, gotSet); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	reread, _ := db.GetTask(ctx, set.ID)
	if reread.ThinkingBudgetTokens == nil || *reread.ThinkingBudgetTokens != 16384 {
		t.Errorf("updated budget = %v, want 16384", reread.ThinkingBudgetTokens)
	}
}
