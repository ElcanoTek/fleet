package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestUpdateTaskSandboxLimits(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	task := &models.Task{
		ID:        uuid.New(),
		Prompt:    "p",
		Status:    models.TaskStatusPending,
		CreatedAt: time.Now().UTC(),
	}
	if _, err := store.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("add: %v", err)
	}

	lim := &models.TaskSandboxLimits{MemoryMB: 2048, CPUs: 2, Pids: 512}
	upd, err := store.UpdateTaskSandboxLimits(ctx, task.ID, lim)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if upd.SandboxLimits == nil || *upd.SandboxLimits != *lim {
		t.Fatalf("set limits = %+v, want %+v", upd.SandboxLimits, lim)
	}

	cleared, err := store.UpdateTaskSandboxLimits(ctx, task.ID, nil)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cleared.SandboxLimits != nil {
		t.Fatalf("clear left %+v", cleared.SandboxLimits)
	}

	running := &models.Task{
		ID:        uuid.New(),
		Prompt:    "p",
		Status:    models.TaskStatusRunning,
		CreatedAt: time.Now().UTC(),
	}
	if _, err := store.AddTaskWithContext(ctx, running); err != nil {
		t.Fatalf("add running: %v", err)
	}
	if _, err := store.UpdateTaskSandboxLimits(ctx, running.ID, lim); err != ErrTaskNotEditable {
		t.Fatalf("running task: got %v, want ErrTaskNotEditable", err)
	}
}
