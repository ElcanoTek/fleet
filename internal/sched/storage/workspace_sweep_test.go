package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestParseMCPRunTaskID(t *testing.T) {
	t.Parallel()

	id := "c8bd6051-8f8b-4999-bb84-175d8a26a068"
	if got, ok := parseMCPRunTaskID("task-" + id); !ok || got != id {
		t.Fatalf("parseMCPRunTaskID(task-id) = %q, %v", got, ok)
	}
	if got, ok := parseMCPRunTaskID("task-" + id + "-sub1"); !ok || got != id {
		t.Fatalf("parseMCPRunTaskID(task-id-sub1) = %q, %v", got, ok)
	}
	if _, ok := parseMCPRunTaskID("chat-" + id); ok {
		t.Fatal("chat- prefix must not match")
	}
	if _, ok := parseMCPRunTaskID("task-not-a-uuid"); ok {
		t.Fatal("non-uuid must not match")
	}
}

func TestSweepTaskWorkspaces(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)
	ctx := context.Background()

	// Active scheduled task
	future := time.Now().Add(time.Hour).UTC()
	activeID := uuid.New()
	activeTask := &models.Task{
		ID:           activeID,
		LineageID:    activeID,
		Prompt:       "active recurring",
		Status:       models.TaskStatusScheduled,
		ScheduledFor: &future,
		Priority:     models.PriorityNormal,
		Recurrence:   "@daily",
		Timezone:     "UTC",
		CreatedAt:    time.Now().UTC(),
	}
	if _, err := s.AddTaskWithContext(ctx, activeTask); err != nil {
		t.Fatal(err)
	}

	// Dead-lettered task (replayable — its mcp-runs ledger and lineage workspace must survive)
	dlqID := uuid.New()
	dlqAt := time.Now().Add(-10 * 24 * time.Hour).UTC()
	dlqTask := &models.Task{
		ID:             dlqID,
		LineageID:      dlqID,
		Prompt:         "replayable dead-lettered run",
		Status:         models.TaskStatusDeadLettered,
		Priority:       models.PriorityNormal,
		Timezone:       "UTC",
		CreatedAt:      dlqAt,
		DeadLetteredAt: &dlqAt,
	}
	if _, err := s.AddTaskWithContext(ctx, dlqTask); err != nil {
		t.Fatal(err)
	}

	// Completed old task (completed 10 days ago)
	doneID := uuid.New()
	doneAt := time.Now().Add(-10 * 24 * time.Hour).UTC()
	doneTask := &models.Task{
		ID:          doneID,
		LineageID:   doneID,
		Prompt:      "finished one-off",
		Status:      models.TaskStatusSuccess,
		Priority:    models.PriorityNormal,
		Timezone:    "UTC",
		CreatedAt:   doneAt,
		CompletedAt: &doneAt,
	}
	if _, err := s.AddTaskWithContext(ctx, doneTask); err != nil {
		t.Fatal(err)
	}

	orphanID := uuid.New().String()

	wsRoot := t.TempDir()
	mcpRuns := filepath.Join(wsRoot, "mcp-runs")
	tasksDir := filepath.Join(wsRoot, "tasks")

	for _, p := range []string{
		filepath.Join(mcpRuns, "task-"+activeTask.ID.String()),
		filepath.Join(mcpRuns, "task-"+dlqTask.ID.String()),
		filepath.Join(mcpRuns, "task-"+doneTask.ID.String()),
		filepath.Join(mcpRuns, "task-"+orphanID),
		filepath.Join(tasksDir, activeTask.WorkspaceLineage().String()),
		filepath.Join(tasksDir, dlqTask.WorkspaceLineage().String()),
		filepath.Join(tasksDir, doneTask.WorkspaceLineage().String()),
		filepath.Join(tasksDir, orphanID),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "marker.txt"), []byte("ok"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := s.SweepTaskWorkspaces(ctx, wsRoot, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("SweepTaskWorkspaces: %v", err)
	}
	if removed != 4 {
		t.Fatalf("removed = %d, want 4 (2 mcp-runs + 2 tasks)", removed)
	}

	// Active and dead-lettered tasks' mcp-runs and tasks dirs survive.
	for _, keep := range []string{
		filepath.Join(mcpRuns, "task-"+activeTask.ID.String()),
		filepath.Join(mcpRuns, "task-"+dlqTask.ID.String()),
		filepath.Join(tasksDir, activeTask.WorkspaceLineage().String()),
		filepath.Join(tasksDir, dlqTask.WorkspaceLineage().String()),
	} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("expected %s to survive: %v", keep, err)
		}
	}

	// Done and orphan mcp-runs + tasks dirs are gone.
	for _, gone := range []string{
		filepath.Join(mcpRuns, "task-"+doneTask.ID.String()),
		filepath.Join(mcpRuns, "task-"+orphanID),
		filepath.Join(tasksDir, doneTask.WorkspaceLineage().String()),
		filepath.Join(tasksDir, orphanID),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, stat err = %v", gone, err)
		}
	}
}
