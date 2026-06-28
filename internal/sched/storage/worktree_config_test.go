package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestWorktreeConfigRoundTrip pins the nullable worktree_config JSONB column
// (#180): nil (shared-workspace task) and a populated config must round-trip
// without corruption, and an edit must be able to both set and clear it.
func TestWorktreeConfigRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// nil → shared-workspace task → reads back nil.
	shared := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, shared); err != nil {
		t.Fatalf("add shared: %v", err)
	}
	if got, err := store.GetTask(shared.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.WorktreeConfig != nil {
		t.Errorf("nil worktree_config must round-trip as nil, got %#v", got.WorktreeConfig)
	}

	// Populated → reads back equal.
	isolated := &models.Task{
		ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(),
		WorktreeConfig: &models.WorktreeConfig{
			Enabled:             true,
			BaseBranch:          "main",
			BranchPrefix:        "iso/",
			AutoCleanup:         true,
			CleanupDelaySeconds: 1800,
		},
	}
	if _, err := store.AddTaskWithContext(ctx, isolated); err != nil {
		t.Fatalf("add isolated: %v", err)
	}
	got, err := store.GetTask(isolated.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WorktreeConfig == nil {
		t.Fatal("populated worktree_config must round-trip as non-nil")
	}
	wc := got.WorktreeConfig
	if !wc.Enabled || wc.BaseBranch != "main" || wc.BranchPrefix != "iso/" || !wc.AutoCleanup || wc.CleanupDelaySeconds != 1800 {
		t.Errorf("worktree_config did not round-trip: %#v", wc)
	}
}

// TestWorktreeConfigEdit exercises the TaskEdit set/clear flag: an edit replaces
// the config (including replacing it with nil to disable isolation).
func TestWorktreeConfigEdit(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	task := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Set a config via edit.
	set := TaskEdit{
		Prompt:            "p",
		WorktreeConfig:    &models.WorktreeConfig{Enabled: true, BranchPrefix: "fleet/task-"},
		SetWorktreeConfig: true,
	}
	if _, err := store.UpdateEditableTask(ctx, task.ID, set); err != nil {
		t.Fatalf("edit set: %v", err)
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WorktreeConfig == nil || !got.WorktreeConfig.Enabled {
		t.Fatalf("edit should have set an enabled worktree_config, got %#v", got.WorktreeConfig)
	}

	// Clear it via edit (replace with nil).
	clrEdit := TaskEdit{Prompt: "p", WorktreeConfig: nil, SetWorktreeConfig: true}
	if _, err := store.UpdateEditableTask(ctx, task.ID, clrEdit); err != nil {
		t.Fatalf("edit clear: %v", err)
	}
	if got, err := store.GetTask(task.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.WorktreeConfig != nil {
		t.Errorf("edit should have cleared worktree_config, got %#v", got.WorktreeConfig)
	}
}
