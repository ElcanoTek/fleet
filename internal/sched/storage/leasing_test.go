package storage

import (
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/google/uuid"
)

func TestTaskLeasing(t *testing.T) {
	store, _ := newTestStore(t)

	node := &models.Node{ID: uuid.New(), Hostname: "test-host", Name: "test-node", APIKey: "test-key", Status: models.NodeStatusIdle, OSType: "linux", LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC()}
	if _, err := store.AddNode(node); err != nil {
		t.Fatalf("Failed to add node: %v", err)
	}

	task := &models.Task{ID: uuid.New(), Prompt: "leasing test task", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}

	// 1. Basic leasing
	assignedTask, err := store.AssignTaskToNode(task.ID, node.ID)
	if err != nil {
		t.Fatalf("Failed to assign task: %v", err)
	}
	if assignedTask.Status != models.TaskStatusLeased {
		t.Errorf("Expected status Leased, got %s", assignedTask.Status)
	}
	if assignedTask.LeaseOwner == nil || *assignedTask.LeaseOwner != node.ID.String() {
		t.Errorf("Expected LeaseOwner %s, got %v", node.ID, assignedTask.LeaseOwner)
	}
	if assignedTask.LeaseExpiresAt == nil || assignedTask.LeaseExpiresAt.Before(time.Now().UTC()) {
		t.Errorf("Invalid LeaseExpiresAt: %v", assignedTask.LeaseExpiresAt)
	}
	if assignedTask.StartedAt != nil {
		t.Error("StartedAt should NOT be set on assignment/leasing")
	}

	// 2. Lease renewal & StartedAt
	shortExpiry := time.Now().UTC().Add(1 * time.Second)
	assignedTask.LeaseExpiresAt = &shortExpiry
	if _, err := store.UpdateTask(assignedTask); err != nil {
		t.Fatalf("Failed to update task expiry: %v", err)
	}
	updatedTask, err := store.UpdateTaskStatusAtomic(assignedTask.ID, node.ID, &models.StatusUpdate{Status: models.TaskStatusRunning})
	if err != nil {
		t.Fatalf("Failed to renew lease: %v", err)
	}
	if updatedTask.LeaseExpiresAt == nil {
		t.Fatal("LeaseExpiresAt is nil after renewal")
	}
	if updatedTask.StartedAt == nil {
		t.Error("StartedAt should be set on first running update")
	}
	if updatedTask.Status != models.TaskStatusRunning {
		t.Errorf("Expected status Running, got %s", updatedTask.Status)
	}
	if !updatedTask.LeaseExpiresAt.After(time.Now().UTC().Add(4 * time.Minute)) {
		t.Errorf("Lease was not extended properly. Expiry: %v", updatedTask.LeaseExpiresAt)
	}

	// 3. Multiple tasks per owner
	task2 := &models.Task{ID: uuid.New(), Prompt: "task 2", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	store.AddTask(task2)
	assignedTask2, err := store.AssignTaskToNode(task2.ID, node.ID)
	if err != nil {
		t.Fatalf("Failed to assign second task: %v", err)
	}
	if assignedTask2 == nil {
		t.Fatal("Failed to assign second task (returned nil)")
	}

	// 4. Expired lease recovery
	expired := time.Now().UTC().Add(-1 * time.Minute)
	assignedTask2.LeaseExpiresAt = &expired
	assignedTask2.Status = models.TaskStatusLeased
	store.UpdateTask(assignedTask2)

	count, err := store.RecoverExpiredLeases()
	if err != nil {
		t.Fatalf("Recovery failed: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected 1 recovered task, got %d", count)
	}

	recoveredTask, _ := store.GetTask(task2.ID)
	if recoveredTask.Status != models.TaskStatusPending {
		t.Errorf("Expected status Pending after recovery, got %s", recoveredTask.Status)
	}
	if recoveredTask.LeaseOwner != nil {
		t.Error("LeaseOwner should be nil after recovery")
	}

	// Reassign to another node
	node2 := &models.Node{ID: uuid.New(), Hostname: "node2", Name: "node2", APIKey: "key2", Status: models.NodeStatusIdle, OSType: "linux", LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC()}
	if _, err := store.AddNode(node2); err != nil {
		t.Fatalf("Failed to add node2: %v", err)
	}
	reassigned, err := store.AssignTaskToNode(task2.ID, node2.ID)
	if err != nil {
		t.Fatalf("Failed to reassign expired task: %v", err)
	}
	if reassigned == nil {
		t.Fatal("Expected reassignment of recovered task")
	}
	if *reassigned.LeaseOwner != node2.ID.String() {
		t.Errorf("Expected owner node2, got %s", *reassigned.LeaseOwner)
	}
}

// TestRecoveredTaskRejectsOldNode verifies that an owner that lost its lease
// (recovery cleared lease_owner + assigned_node_id) cannot update the task
// status, preventing two workers from running the same task. Adapted from moc:
// the wildcard glob-routing setup is removed (the synthetic worker claims tasks
// directly), but the lease-ownership rejection contract is identical.
func TestRecoveredTaskRejectsOldNode(t *testing.T) {
	store, _ := newTestStore(t)

	nodeA := &models.Node{ID: uuid.New(), Hostname: "host-a", Name: "worker-a", APIKey: "key-a", Status: models.NodeStatusIdle, OSType: "linux", LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC()}
	nodeB := &models.Node{ID: uuid.New(), Hostname: "host-b", Name: "worker-b", APIKey: "key-b", Status: models.NodeStatusIdle, OSType: "linux", LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC()}
	if _, err := store.AddNode(nodeA); err != nil {
		t.Fatalf("Failed to add nodeA: %v", err)
	}
	if _, err := store.AddNode(nodeB); err != nil {
		t.Fatalf("Failed to add nodeB: %v", err)
	}

	task := &models.Task{ID: uuid.New(), Prompt: "race condition test", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}

	// Node A leases the task.
	assignedTask, err := store.AssignTaskToNode(task.ID, nodeA.ID)
	if err != nil {
		t.Fatalf("Failed to assign task to nodeA: %v", err)
	}
	if assignedTask == nil || *assignedTask.LeaseOwner != nodeA.ID.String() {
		t.Fatalf("Expected task leased to nodeA")
	}

	// Force lease expiry and recover (clears lease_owner + assigned_node_id).
	expired := time.Now().UTC().Add(-1 * time.Minute)
	assignedTask.LeaseExpiresAt = &expired
	if _, err := store.UpdateTask(assignedTask); err != nil {
		t.Fatalf("Failed to set expired lease: %v", err)
	}
	count, err := store.RecoverExpiredLeases()
	if err != nil {
		t.Fatalf("RecoverExpiredLeases failed: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected 1 recovered task, got %d", count)
	}

	recoveredTask, _ := store.GetTask(task.ID)
	if recoveredTask.Status != models.TaskStatusPending {
		t.Fatalf("Expected pending after recovery, got %s", recoveredTask.Status)
	}
	if recoveredTask.LeaseOwner != nil || recoveredTask.AssignedNodeID != nil {
		t.Fatal("Expected lease_owner and assigned_node_id to be nil after recovery")
	}

	// Node A (lost lease) cannot report status.
	update := &models.StatusUpdate{Status: models.TaskStatusRunning}
	if _, err := store.UpdateTaskStatusAtomic(task.ID, nodeA.ID, update); err == nil {
		t.Fatal("Expected error when node without lease tries to update task status")
	} else if err.Error() != "node is not assigned to this task" {
		t.Errorf("Expected lease-rejection error, got '%s'", err.Error())
	}

	taskAfter, _ := store.GetTask(task.ID)
	if taskAfter.Status != models.TaskStatusPending {
		t.Errorf("Task status should still be pending, got %s", taskAfter.Status)
	}

	// Node B claims the recovered task.
	assignedToB, err := store.AssignTaskToNode(task.ID, nodeB.ID)
	if err != nil {
		t.Fatalf("Failed to assign task to nodeB: %v", err)
	}
	if assignedToB == nil || *assignedToB.LeaseOwner != nodeB.ID.String() {
		t.Fatal("Expected nodeB to claim the recovered task")
	}

	// Node A still rejected; node B accepted.
	if _, err := store.UpdateTaskStatusAtomic(task.ID, nodeA.ID, update); err == nil {
		t.Fatal("Expected error when nodeA updates task owned by nodeB")
	}
	updatedByB, err := store.UpdateTaskStatusAtomic(task.ID, nodeB.ID, update)
	if err != nil {
		t.Fatalf("NodeB should be able to update its own task: %v", err)
	}
	if updatedByB.Status != models.TaskStatusRunning {
		t.Errorf("Expected status running, got %s", updatedByB.Status)
	}
}

// TestRecoverExpiredLeasesSelectivity pins the recovery predicate's
// selectivity: RecoverExpiredLeases must re-queue ONLY genuinely-expired active
// leases (status in leased/running/analyzing AND lease_expires_at < now). A
// not-yet-expired lease, a terminal task, and a plain pending task must all be
// left untouched — so the crash-safe backstop never steals a live worker's task
// nor resurrects a finished one. The existing TestTaskLeasing only asserts the
// recovered count for a single expired task; this isolates the negative cases in
// one mixed pending set.
func TestRecoverExpiredLeasesSelectivity(t *testing.T) {
	past := time.Now().UTC().Add(-time.Minute)
	future := time.Now().UTC().Add(LeaseDuration)

	cases := []struct {
		name          string
		status        models.TaskStatus
		leaseExpires  *time.Time
		wantRecovered bool // becomes pending with cleared lease
	}{
		{"expired-leased", models.TaskStatusLeased, &past, true},
		{"expired-running", models.TaskStatusRunning, &past, true},
		{"expired-analyzing", models.TaskStatusAnalyzing, &past, true},
		{"live-running-not-expired", models.TaskStatusRunning, &future, false},
		{"live-leased-not-expired", models.TaskStatusLeased, &future, false},
		{"terminal-success-stale-lease", models.TaskStatusSuccess, &past, false},
		{"plain-pending-no-lease", models.TaskStatusPending, nil, false},
	}

	store, _ := newTestStore(t)

	owner := uuid.New().String()
	ids := make(map[string]uuid.UUID, len(cases))
	for _, tc := range cases {
		task := &models.Task{
			ID:             uuid.New(),
			Prompt:         tc.name,
			Status:         tc.status,
			Priority:       1,
			CreatedAt:      time.Now().UTC(),
			LeaseExpiresAt: tc.leaseExpires,
		}
		if tc.leaseExpires != nil {
			o := owner
			task.LeaseOwner = &o
		}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("%s: AddTask: %v", tc.name, err)
		}
		ids[tc.name] = task.ID
	}

	wantCount := 0
	for _, tc := range cases {
		if tc.wantRecovered {
			wantCount++
		}
	}

	got, err := store.RecoverExpiredLeases()
	if err != nil {
		t.Fatalf("RecoverExpiredLeases: %v", err)
	}
	if got != wantCount {
		t.Fatalf("recovered %d tasks, want exactly %d (only genuinely-expired active leases)", got, wantCount)
	}

	for _, tc := range cases {
		after, err := store.GetTask(ids[tc.name])
		if err != nil {
			t.Fatalf("%s: GetTask: %v", tc.name, err)
		}
		if tc.wantRecovered {
			if after.Status != models.TaskStatusPending {
				t.Errorf("%s: status = %s after recovery, want pending", tc.name, after.Status)
			}
			if after.LeaseOwner != nil || after.LeaseExpiresAt != nil {
				t.Errorf("%s: lease not cleared after recovery: owner=%v expiry=%v", tc.name, after.LeaseOwner, after.LeaseExpiresAt)
			}
		} else if after.Status != tc.status {
			t.Errorf("%s: status = %s, want it LEFT as %s (recovery over-reached)", tc.name, after.Status, tc.status)
		}
	}
}

func TestTaskLeasingUsesFixedLeaseWindow(t *testing.T) {
	store, _ := newTestStore(t)

	node := &models.Node{ID: uuid.New(), Hostname: "test-host-custom", Name: "test-node-custom", APIKey: "test-key-custom", Status: models.NodeStatusIdle, OSType: "linux", LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC()}
	if _, err := store.AddNode(node); err != nil {
		t.Fatalf("Failed to add node: %v", err)
	}

	task := &models.Task{ID: uuid.New(), Prompt: "leasing fixed-window task", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}

	assignedTask, err := store.AssignTaskToNode(task.ID, node.ID)
	if err != nil {
		t.Fatalf("Failed to assign task: %v", err)
	}

	now := time.Now().UTC()
	if assignedTask.LeaseExpiresAt.Before(now.Add(4 * time.Minute)) {
		t.Errorf("Lease expiry too short. Expected ~5m, got %v", assignedTask.LeaseExpiresAt)
	}
	if assignedTask.LeaseExpiresAt.After(now.Add(6 * time.Minute)) {
		t.Errorf("Lease expiry too long. Expected ~5m, got %v", assignedTask.LeaseExpiresAt)
	}

	updatedTask, err := store.UpdateTaskStatusAtomic(assignedTask.ID, node.ID, &models.StatusUpdate{Status: models.TaskStatusRunning})
	if err != nil {
		t.Fatalf("Failed to update status: %v", err)
	}
	if updatedTask.LeaseExpiresAt.Before(time.Now().UTC().Add(4 * time.Minute)) {
		t.Errorf("Lease expiry not extended properly. Expected ~5m, got %v", updatedTask.LeaseExpiresAt)
	}
}
