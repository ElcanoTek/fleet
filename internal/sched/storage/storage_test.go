package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func acquireTestLock(t *testing.T, db *db.Database) {
	ctx := context.Background()
	conn, err := db.Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("Failed to get DB connection for lock: %v", err)
	}
	// Serialize all sched tests sharing the DB on a fixed advisory lock.
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(1)"); err != nil {
		conn.Close()
		t.Fatalf("Failed to acquire test lock: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock(1)"); err != nil {
			t.Logf("Failed to release test lock: %v", err)
		}
		conn.Close()
	})
}

func newTestStore(t *testing.T) (*Storage, *db.Database) {
	t.Helper()
	database := db.New()
	if err := database.Init("", db.DefaultPoolConfig()); err != nil {
		t.Skipf("Skipping storage test because DB init failed: %v", err)
	}

	acquireTestLock(t, database)

	ctx := context.Background()
	cleanup := func() {
		for _, q := range []string{"DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM users"} {
			database.Conn().ExecContext(ctx, q)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	store := New()
	store.SetDatabase(database)
	t.Cleanup(func() { database.Close() })
	return store, database
}

func TestStorage(t *testing.T) {
	store, _ := newTestStore(t)

	// MatchGlob (the surviving scope-matching helper).
	if !MatchGlob("foo*", "foobar") {
		t.Error("MatchGlob failed")
	}
	if MatchGlob("foo*", "barfoo") {
		t.Error("MatchGlob matched incorrectly")
	}

	// A pending task is claimed via ClaimNextPendingTask (no node routing).
	taskPending := &models.Task{ID: uuid.New(), Prompt: "pending task", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(taskPending); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}

	claimed, err := store.ClaimNextPendingTask(context.Background(), "synthetic-worker")
	if err != nil {
		t.Fatalf("Failed to claim pending task: %v", err)
	}
	if claimed == nil || claimed.ID != taskPending.ID {
		t.Fatalf("Expected to claim taskPending, got %v", claimed)
	}
	if claimed.Status != models.TaskStatusLeased {
		t.Errorf("Expected status Leased, got %s", claimed.Status)
	}

	// No more pending tasks → nil.
	again, err := store.ClaimNextPendingTask(context.Background(), "synthetic-worker")
	if err != nil {
		t.Fatalf("Second claim failed: %v", err)
	}
	if again != nil {
		t.Error("Should not claim an already-leased task")
	}
}

func TestRecurringTaskRescheduling(t *testing.T) {
	store, database := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	owner := uuid.New()

	recurringTask := &models.Task{ID: uuid.New(), Prompt: "recurring test task", Status: models.TaskStatusPending, Priority: 10, Recurrence: "@daily", CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(recurringTask); err != nil {
		t.Fatalf("Failed to add recurring task: %v", err)
	}

	assignedTask, err := store.leaseTaskToOwner(recurringTask.ID, owner)
	if err != nil {
		t.Fatalf("Failed to lease task: %v", err)
	}
	if assignedTask.Status != models.TaskStatusLeased {
		t.Errorf("Expected status Leased, got %s", assignedTask.Status)
	}

	completedTask, err := store.UpdateTaskStatusAtomic(assignedTask.ID, owner, &models.StatusUpdate{Status: models.TaskStatusSuccess, Message: strPtr("done")})
	if err != nil {
		t.Fatalf("Failed to update task status: %v", err)
	}
	if completedTask.Status != models.TaskStatusSuccess {
		t.Errorf("Expected status Success, got %s", completedTask.Status)
	}

	allTasks, err := store.GetAllTasks()
	if err != nil {
		t.Fatalf("Failed to get all tasks: %v", err)
	}
	if len(allTasks) != 2 {
		t.Fatalf("Expected 2 tasks (original + next occurrence), got %d", len(allTasks))
	}

	var nextTask *models.Task
	for _, task := range allTasks {
		if task.ID != recurringTask.ID {
			nextTask = task
			break
		}
	}
	if nextTask == nil {
		t.Fatal("Next recurring task not found")
	}
	if nextTask.Prompt != recurringTask.Prompt {
		t.Errorf("Expected prompt '%s', got '%s'", recurringTask.Prompt, nextTask.Prompt)
	}
	if nextTask.Recurrence != recurringTask.Recurrence {
		t.Errorf("Expected recurrence '%s', got '%s'", recurringTask.Recurrence, nextTask.Recurrence)
	}
	if nextTask.Status != models.TaskStatusScheduled {
		t.Errorf("Expected status Scheduled, got %s", nextTask.Status)
	}
	if nextTask.ScheduledFor == nil {
		t.Error("Next task has no scheduled_for time")
	} else if !nextTask.ScheduledFor.After(time.Now().UTC()) {
		t.Errorf("Next task scheduled in the past: %s", nextTask.ScheduledFor)
	}

	// Error status also reschedules.
	recurringTask2 := &models.Task{ID: uuid.New(), Prompt: "recurring task that fails", Status: models.TaskStatusPending, Priority: 10, Recurrence: "@hourly", CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(recurringTask2); err != nil {
		t.Fatalf("Failed to add second recurring task: %v", err)
	}
	assignedTask2, err := store.leaseTaskToOwner(recurringTask2.ID, owner)
	if err != nil {
		t.Fatalf("Failed to lease second task: %v", err)
	}
	_, err = store.UpdateTaskStatusAtomic(assignedTask2.ID, owner, &models.StatusUpdate{Status: models.TaskStatusError, Message: strPtr("Task failed")})
	if err != nil {
		t.Fatalf("Failed to update task status: %v", err)
	}

	var taskCount int
	if err := database.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks").Scan(&taskCount); err != nil {
		t.Fatalf("Failed to count tasks: %v", err)
	}
	allTasks, err = store.GetAllTasks()
	if err != nil {
		t.Fatalf("Failed to get all tasks: %v", err)
	}
	if len(allTasks) != 4 {
		t.Errorf("Expected 4 tasks total, got %d (direct count: %d)", len(allTasks), taskCount)
	}
}

// TestRecurringTaskPreservesTimezone verifies the recurrence chain keeps the
// task's IANA timezone (so a "9am New York" task keeps firing at 9am New York
// rather than reverting to the server-global zone). Server tz is deliberately
// set to a DIFFERENT zone to prove the per-task value, not s.location, is used.
func TestRecurringTaskPreservesTimezone(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")

	owner := uuid.New()

	const tz = "America/New_York"
	task := &models.Task{ID: uuid.New(), Prompt: "tz recur", Status: models.TaskStatusPending, Priority: 5, Recurrence: "@daily", Timezone: tz, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	assigned, err := store.leaseTaskToOwner(task.ID, owner)
	if err != nil {
		t.Fatalf("leaseTaskToOwner: %v", err)
	}
	if _, err := store.UpdateTaskStatusAtomic(assigned.ID, owner, &models.StatusUpdate{Status: models.TaskStatusSuccess, Message: strPtr("done")}); err != nil {
		t.Fatalf("UpdateTaskStatusAtomic: %v", err)
	}

	all, err := store.GetAllTasks()
	if err != nil {
		t.Fatalf("GetAllTasks: %v", err)
	}
	var next *models.Task
	for _, tk := range all {
		if tk.ID != task.ID {
			next = tk
			break
		}
	}
	if next == nil {
		t.Fatal("next recurrence not created")
	}
	if next.Timezone != tz {
		t.Errorf("next occurrence Timezone = %q, want %q", next.Timezone, tz)
	}
}

func TestUpdateTaskStatusAtomicIgnoresStaleRunningAfterSuccess(t *testing.T) {
	store, _ := newTestStore(t)

	owner := uuid.New()

	task := &models.Task{ID: uuid.New(), Prompt: "stale-running-regression", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}

	assignedTask, err := store.leaseTaskToOwner(task.ID, owner)
	if err != nil {
		t.Fatalf("Failed to lease task: %v", err)
	}

	successMessage := "Task completed successfully"
	completedTask, err := store.UpdateTaskStatusAtomic(assignedTask.ID, owner, &models.StatusUpdate{Status: models.TaskStatusSuccess, Message: &successMessage})
	if err != nil {
		t.Fatalf("Failed to mark task successful: %v", err)
	}
	if completedTask.Status != models.TaskStatusSuccess {
		t.Fatalf("Expected task status success, got %s", completedTask.Status)
	}
	if completedTask.CompletedAt == nil {
		t.Fatal("Expected completed_at to be set")
	}

	// A stale Running update arriving after success must NOT resurrect the task.
	// Completing the task cleared its lease owner, so a late update from the same
	// worker no longer holds the lease and is rejected outright — a strictly
	// stronger guard than the old silent no-op. Either way the terminal task is
	// never reopened.
	staleMessage := "Starting task execution"
	if _, err := store.UpdateTaskStatusAtomic(assignedTask.ID, owner, &models.StatusUpdate{Status: models.TaskStatusRunning, Message: &staleMessage}); err == nil {
		t.Fatal("Expected stale running update on a completed task to be rejected (lease cleared on success)")
	} else if err.Error() != "worker does not hold the lease on this task" {
		t.Fatalf("Expected lease-rejection error, got %q", err.Error())
	}

	reloadedTask, err := store.GetTask(assignedTask.ID)
	if err != nil {
		t.Fatalf("Failed to reload task: %v", err)
	}
	if reloadedTask.Status != models.TaskStatusSuccess {
		t.Fatalf("Expected persisted task status success, got %s", reloadedTask.Status)
	}
	if reloadedTask.LeaseOwner != nil || reloadedTask.LeaseExpiresAt != nil {
		t.Fatalf("Expected completed task lease cleared, got owner=%v expiry=%v", reloadedTask.LeaseOwner, reloadedTask.LeaseExpiresAt)
	}
	if reloadedTask.Result == nil || *reloadedTask.Result != successMessage {
		t.Fatalf("Expected success result %q, got %v", successMessage, reloadedTask.Result)
	}
}

// TestTerminalTransitionComputesActualDuration (#274) confirms the SLA
// actual-duration field is populated when a task reaches a terminal status
// (StartedAt + CompletedAt both present), so the SLA report has real data to
// aggregate. It also confirms an SLA-configured task round-trips its expected
// duration + multipliers through the DB.
func TestTerminalTransitionComputesActualDuration(t *testing.T) {
	store, _ := newTestStore(t)

	owner := uuid.New()

	expected := 15
	task := &models.Task{
		ID:                      uuid.New(),
		Prompt:                  "sla-round-trip",
		Status:                  models.TaskStatusPending,
		Priority:                1,
		CreatedAt:               time.Now().UTC(),
		ExpectedDurationMinutes: &expected,
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}

	assigned, err := store.leaseTaskToOwner(task.ID, owner)
	if err != nil {
		t.Fatalf("Failed to lease task: %v", err)
	}

	// Promote to running so StartedAt is recorded, then to success so
	// CompletedAt is recorded and the actual duration is derived.
	if _, err := store.UpdateTaskStatusAtomic(assigned.ID, owner, &models.StatusUpdate{Status: models.TaskStatusRunning}); err != nil {
		t.Fatalf("running update failed: %v", err)
	}
	// Force a non-trivial StartedAt gap so the derived seconds are > 0.
	running, err := store.GetTask(assigned.ID)
	if err != nil || running.StartedAt == nil {
		t.Fatalf("expected StartedAt set, err=%v", err)
	}
	startedCopy := running.StartedAt.Add(-90 * time.Second) // 90s ago
	running.StartedAt = &startedCopy
	if _, err := store.UpdateTask(running); err != nil {
		t.Fatalf("UpdateTask (back-dated start) failed: %v", err)
	}
	completed, err := store.UpdateTaskStatusAtomic(assigned.ID, owner, &models.StatusUpdate{Status: models.TaskStatusSuccess})
	if err != nil {
		t.Fatalf("success update failed: %v", err)
	}
	if completed.ActualDurationSeconds == nil || *completed.ActualDurationSeconds < 60 {
		t.Fatalf("expected ActualDurationSeconds >= 60, got %v", completed.ActualDurationSeconds)
	}

	// Round-trip the SLA config: reload and confirm expected_duration_minutes
	// + the default multipliers survived the write/read.
	reloaded, err := store.GetTask(assigned.ID)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.ExpectedDurationMinutes == nil || *reloaded.ExpectedDurationMinutes != expected {
		t.Fatalf("expected_duration_minutes round-trip = %v, want %d", reloaded.ExpectedDurationMinutes, expected)
	}
	if reloaded.SLAWarnMultiplier != models.DefaultSLAWarnMultiplier {
		t.Fatalf("sla_warn_multiplier = %v, want default %.2f", reloaded.SLAWarnMultiplier, models.DefaultSLAWarnMultiplier)
	}
	if reloaded.SLAFailMultiplier != models.DefaultSLAFailMultiplier {
		t.Fatalf("sla_fail_multiplier = %v, want default %.2f", reloaded.SLAFailMultiplier, models.DefaultSLAFailMultiplier)
	}
}

func strPtr(s string) *string { return &s }
