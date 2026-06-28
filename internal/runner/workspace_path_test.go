package runner

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestReportWorkspacePath_Persists verifies the pool persists the per-run
// workspace path (#287) onto the leased task row, and that a later terminal
// status update does NOT wipe it (the nil-WorkspacePath guard in storage).
func TestReportWorkspacePath_Persists(t *testing.T) {
	store := newTestStore(t)

	task := &models.Task{ID: uuid.New(), Prompt: "produce report", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	pool := NewPool(store, nil, Config{MaxConcurrentAgents: 1})

	// Lease the task to this pool's synthetic worker so the atomic
	// lease-ownership check in UpdateTaskStatusAtomic admits our writes.
	claimed, err := store.ClaimNextPendingTask(context.Background(), pool.LeaseOwner().String())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != task.ID {
		t.Fatalf("claim returned %v; want task %s", claimed, task.ID)
	}

	const wsPath = "/var/lib/fleet/workspace/cutlass-run-abc123"
	pool.reportWorkspacePath(task.ID, wsPath)

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.WorkspacePath == nil || *got.WorkspacePath != wsPath {
		t.Fatalf("WorkspacePath = %v; want %q", got.WorkspacePath, wsPath)
	}

	// A terminal status update with no WorkspacePath must leave it intact.
	if _, err := pool.reportStatus(task.ID, models.TaskStatusSuccess, "done"); err != nil {
		t.Fatalf("report success: %v", err)
	}
	got, err = store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task after success: %v", err)
	}
	if got.Status != models.TaskStatusSuccess {
		t.Fatalf("status = %s; want success", got.Status)
	}
	if got.WorkspacePath == nil || *got.WorkspacePath != wsPath {
		t.Fatalf("WorkspacePath wiped by terminal update: %v", got.WorkspacePath)
	}
}

// TestReportWorkspacePath_EmptyNoop verifies an empty path is never written.
func TestReportWorkspacePath_EmptyNoop(t *testing.T) {
	store := newTestStore(t)
	task := &models.Task{ID: uuid.New(), Prompt: "x", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	pool := NewPool(store, nil, Config{MaxConcurrentAgents: 1})
	if _, err := store.ClaimNextPendingTask(context.Background(), pool.LeaseOwner().String()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	pool.reportWorkspacePath(task.ID, "   ") // whitespace-only → no-op
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WorkspacePath != nil {
		t.Fatalf("empty path should not be persisted; got %q", *got.WorkspacePath)
	}
}
