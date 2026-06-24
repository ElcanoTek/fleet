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
	if err := database.Init(""); err != nil {
		t.Skipf("Skipping storage test because DB init failed: %v", err)
	}

	acquireTestLock(t, database)

	ctx := context.Background()
	cleanup := func() {
		for _, q := range []string{"DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM nodes", "DELETE FROM users"} {
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

	node := &models.Node{ID: uuid.New(), Name: "client-acme-prod-01", Hostname: "h", APIKey: "k", Status: models.NodeStatusIdle, OSType: "linux", LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC()}
	if _, err := store.AddNode(node); err != nil {
		t.Fatalf("Failed to add node: %v", err)
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

	node := &models.Node{ID: uuid.New(), Hostname: "test-host", Name: "test-node", APIKey: "test-key", Status: models.NodeStatusIdle, OSType: "linux", LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC()}
	if _, err := store.AddNode(node); err != nil {
		t.Fatalf("Failed to add node: %v", err)
	}

	recurringTask := &models.Task{ID: uuid.New(), Prompt: "recurring test task", Status: models.TaskStatusPending, Priority: 10, Recurrence: "@daily", CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(recurringTask); err != nil {
		t.Fatalf("Failed to add recurring task: %v", err)
	}

	assignedTask, err := store.AssignTaskToNode(recurringTask.ID, node.ID)
	if err != nil {
		t.Fatalf("Failed to assign task: %v", err)
	}
	if assignedTask.Status != models.TaskStatusLeased {
		t.Errorf("Expected status Leased, got %s", assignedTask.Status)
	}

	completedTask, err := store.UpdateTaskStatusAtomic(assignedTask.ID, node.ID, &models.StatusUpdate{Status: models.TaskStatusSuccess, Message: strPtr("done")})
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
	assignedTask2, err := store.AssignTaskToNode(recurringTask2.ID, node.ID)
	if err != nil {
		t.Fatalf("Failed to assign second task: %v", err)
	}
	_, err = store.UpdateTaskStatusAtomic(assignedTask2.ID, node.ID, &models.StatusUpdate{Status: models.TaskStatusError, Message: strPtr("Task failed")})
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

func TestUpdateNodeHeartbeatRenewsActiveTaskLease(t *testing.T) {
	store, _ := newTestStore(t)

	node := &models.Node{ID: uuid.New(), Hostname: "test-node", Name: "test-node", APIKey: uuid.New().String(), OSType: "linux", Status: models.NodeStatusIdle, LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC()}
	if _, err := store.AddNode(node); err != nil {
		t.Fatalf("Failed to add node: %v", err)
	}

	task := &models.Task{ID: uuid.New(), Prompt: "lease-renewal test", Status: models.TaskStatusRunning, AssignedNodeID: &node.ID, CreatedAt: time.Now().UTC()}
	leaseOwner := node.ID.String()
	originalExpiry := time.Now().UTC().Add(30 * time.Second)
	task.LeaseOwner = &leaseOwner
	task.LeaseExpiresAt = &originalExpiry
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}

	updatedNode, err := store.UpdateNodeHeartbeat(node.ID, models.NodeStatusBusy, &task.ID)
	if err != nil {
		t.Fatalf("UpdateNodeHeartbeat failed: %v", err)
	}
	if updatedNode.CurrentTaskID == nil || *updatedNode.CurrentTaskID != task.ID {
		t.Fatalf("expected node current task %s, got %v", task.ID, updatedNode.CurrentTaskID)
	}

	updatedTask, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("Failed to reload task: %v", err)
	}
	if updatedTask.LeaseExpiresAt == nil {
		t.Fatal("expected lease expiry to remain set")
	}
	if !updatedTask.LeaseExpiresAt.After(originalExpiry) {
		t.Fatalf("expected heartbeat to extend lease beyond %v, got %v", originalExpiry, updatedTask.LeaseExpiresAt)
	}

	updatedTask.Status = models.TaskStatusPending
	updatedTask.AssignedNodeID = nil
	updatedTask.LeaseOwner = nil
	updatedTask.LeaseExpiresAt = nil
	if _, err := store.UpdateTask(updatedTask); err != nil {
		t.Fatalf("Failed to simulate recovered task: %v", err)
	}

	if _, err := store.UpdateNodeHeartbeat(node.ID, models.NodeStatusBusy, &task.ID); err != nil {
		t.Fatalf("Second UpdateNodeHeartbeat failed: %v", err)
	}

	recoveredTask, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("Failed to reload recovered task: %v", err)
	}
	if recoveredTask.LeaseExpiresAt != nil || recoveredTask.LeaseOwner != nil || recoveredTask.AssignedNodeID != nil {
		t.Fatalf("expected recovered task to remain unowned, got lease=%v owner=%v assigned=%v", recoveredTask.LeaseExpiresAt, recoveredTask.LeaseOwner, recoveredTask.AssignedNodeID)
	}
}

func TestUpdateTaskStatusAtomicIgnoresStaleRunningAfterSuccess(t *testing.T) {
	store, _ := newTestStore(t)

	node := &models.Node{ID: uuid.New(), Hostname: "test-node", Name: "test-node", APIKey: uuid.New().String(), OSType: "linux", Status: models.NodeStatusIdle, LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC()}
	if _, err := store.AddNode(node); err != nil {
		t.Fatalf("Failed to add node: %v", err)
	}

	task := &models.Task{ID: uuid.New(), Prompt: "stale-running-regression", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}

	assignedTask, err := store.AssignTaskToNode(task.ID, node.ID)
	if err != nil {
		t.Fatalf("Failed to assign task: %v", err)
	}

	successMessage := "Task completed successfully"
	completedTask, err := store.UpdateTaskStatusAtomic(assignedTask.ID, node.ID, &models.StatusUpdate{Status: models.TaskStatusSuccess, Message: &successMessage})
	if err != nil {
		t.Fatalf("Failed to mark task successful: %v", err)
	}
	if completedTask.Status != models.TaskStatusSuccess {
		t.Fatalf("Expected task status success, got %s", completedTask.Status)
	}
	if completedTask.CompletedAt == nil {
		t.Fatal("Expected completed_at to be set")
	}

	staleMessage := "Starting task execution"
	staleUpdate, err := store.UpdateTaskStatusAtomic(assignedTask.ID, node.ID, &models.StatusUpdate{Status: models.TaskStatusRunning, Message: &staleMessage})
	if err != nil {
		t.Fatalf("Failed to apply stale running update: %v", err)
	}
	if staleUpdate.Status != models.TaskStatusSuccess {
		t.Fatalf("Expected stale update to preserve success, got %s", staleUpdate.Status)
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

func strPtr(s string) *string { return &s }
