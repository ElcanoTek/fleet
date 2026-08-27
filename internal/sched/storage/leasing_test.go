package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/google/uuid"
)

func TestTaskLeasing(t *testing.T) {
	store, _ := newTestStore(t)

	owner := uuid.New()

	task := &models.Task{ID: uuid.New(), Prompt: "leasing test task", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}

	// 1. Basic leasing
	assignedTask, err := store.leaseTaskToOwner(task.ID, owner)
	if err != nil {
		t.Fatalf("Failed to lease task: %v", err)
	}
	if assignedTask.Status != models.TaskStatusLeased {
		t.Errorf("Expected status Leased, got %s", assignedTask.Status)
	}
	if assignedTask.LeaseOwner == nil || *assignedTask.LeaseOwner != owner.String() {
		t.Errorf("Expected LeaseOwner %s, got %v", owner, assignedTask.LeaseOwner)
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
	updatedTask, err := store.UpdateTaskStatusAtomic(assignedTask.ID, owner, &models.StatusUpdate{Status: models.TaskStatusRunning})
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
	// MaxRetries 1 so recovery re-queues (it dead-letters at attempt_count >= max_retries, #1116).
	task2 := &models.Task{ID: uuid.New(), Prompt: "task 2", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), MaxRetries: 1}
	store.AddTask(task2)
	assignedTask2, err := store.leaseTaskToOwner(task2.ID, owner)
	if err != nil {
		t.Fatalf("Failed to lease second task: %v", err)
	}
	if assignedTask2 == nil {
		t.Fatal("Failed to lease second task (returned nil)")
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

	// Re-lease to another owner
	owner2 := uuid.New()
	reassigned, err := store.leaseTaskToOwner(task2.ID, owner2)
	if err != nil {
		t.Fatalf("Failed to re-lease expired task: %v", err)
	}
	if reassigned == nil {
		t.Fatal("Expected re-lease of recovered task")
	}
	if *reassigned.LeaseOwner != owner2.String() {
		t.Errorf("Expected owner %s, got %s", owner2, *reassigned.LeaseOwner)
	}
}

// TestRecoveredTaskRejectsOldNode verifies that an owner that lost its lease
// (recovery cleared lease_owner) cannot update the task status, preventing two
// workers from running the same task. Adapted from moc: the wildcard
// glob-routing setup is removed (the synthetic worker claims tasks directly),
// but the lease-ownership rejection contract is identical.
func TestRecoveredTaskRejectsOldNode(t *testing.T) {
	store, _ := newTestStore(t)

	ownerA := uuid.New()
	ownerB := uuid.New()

	// MaxRetries 1 so recovery re-queues (it dead-letters at attempt_count >= max_retries, #1116).
	task := &models.Task{ID: uuid.New(), Prompt: "race condition test", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC(), MaxRetries: 1}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}

	// Owner A leases the task.
	assignedTask, err := store.leaseTaskToOwner(task.ID, ownerA)
	if err != nil {
		t.Fatalf("Failed to lease task to ownerA: %v", err)
	}
	if assignedTask == nil || *assignedTask.LeaseOwner != ownerA.String() {
		t.Fatalf("Expected task leased to ownerA")
	}

	// Force lease expiry and recover (clears lease_owner).
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
	if recoveredTask.LeaseOwner != nil {
		t.Fatal("Expected lease_owner to be nil after recovery")
	}

	// Owner A (lost lease) cannot report status.
	update := &models.StatusUpdate{Status: models.TaskStatusRunning}
	if _, err := store.UpdateTaskStatusAtomic(task.ID, ownerA, update); err == nil {
		t.Fatal("Expected error when owner without lease tries to update task status")
	} else if err.Error() != "worker does not hold the lease on this task" {
		t.Errorf("Expected lease-rejection error, got '%s'", err.Error())
	}

	taskAfter, _ := store.GetTask(task.ID)
	if taskAfter.Status != models.TaskStatusPending {
		t.Errorf("Task status should still be pending, got %s", taskAfter.Status)
	}

	// Owner B claims the recovered task.
	assignedToB, err := store.leaseTaskToOwner(task.ID, ownerB)
	if err != nil {
		t.Fatalf("Failed to lease task to ownerB: %v", err)
	}
	if assignedToB == nil || *assignedToB.LeaseOwner != ownerB.String() {
		t.Fatal("Expected ownerB to claim the recovered task")
	}

	// Owner A still rejected; owner B accepted.
	if _, err := store.UpdateTaskStatusAtomic(task.ID, ownerA, update); err == nil {
		t.Fatal("Expected error when ownerA updates task owned by ownerB")
	}
	updatedByB, err := store.UpdateTaskStatusAtomic(task.ID, ownerB, update)
	if err != nil {
		t.Fatalf("OwnerB should be able to update its own task: %v", err)
	}
	if updatedByB.Status != models.TaskStatusRunning {
		t.Errorf("Expected status running, got %s", updatedByB.Status)
	}
}

// TestRecoverExpiredLeasesSelectivity pins the recovery predicate's
// selectivity: RecoverExpiredLeases must re-queue ONLY genuinely-expired active
// leases (status in leased/running AND lease_expires_at < now). A
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
			// Within the retry budget, so the recovered rows re-queue rather
			// than dead-letter (#1116 quarantines at attempt_count >= max_retries).
			MaxRetries: 1,
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

	owner := uuid.New()

	task := &models.Task{ID: uuid.New(), Prompt: "leasing fixed-window task", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}

	assignedTask, err := store.leaseTaskToOwner(task.ID, owner)
	if err != nil {
		t.Fatalf("Failed to lease task: %v", err)
	}

	now := time.Now().UTC()
	if assignedTask.LeaseExpiresAt.Before(now.Add(4 * time.Minute)) {
		t.Errorf("Lease expiry too short. Expected ~5m, got %v", assignedTask.LeaseExpiresAt)
	}
	if assignedTask.LeaseExpiresAt.After(now.Add(6 * time.Minute)) {
		t.Errorf("Lease expiry too long. Expected ~5m, got %v", assignedTask.LeaseExpiresAt)
	}

	updatedTask, err := store.UpdateTaskStatusAtomic(assignedTask.ID, owner, &models.StatusUpdate{Status: models.TaskStatusRunning})
	if err != nil {
		t.Fatalf("Failed to update status: %v", err)
	}
	if updatedTask.LeaseExpiresAt.Before(time.Now().UTC().Add(4 * time.Minute)) {
		t.Errorf("Lease expiry not extended properly. Expected ~5m, got %v", updatedTask.LeaseExpiresAt)
	}
}

// TestLeaseGuardedWritersReturnLeaseSentinel pins the error IDENTITY, not just
// the message text, on every lease-guarded transition writer.
//
// UpdateTaskStatusAtomicWithContext returned ErrTaskLeaseNotHeld, but
// RequeueTaskForRetryWithContext and DeadLetterTaskWithContext each rebuilt the
// SAME message with fmt.Errorf instead of returning the sentinel — so
// errors.Is worked on one of the three lease guards and silently failed on the
// other two. The runner already branches on errors.Is(err,
// ErrTaskLeaseNotHeld) for lease renewals and the success commit; anyone
// extending that treatment to the retry/dead-letter paths would have gotten a
// false negative from an error whose text says exactly what happened. Found
// while working #1268/#1269 (PR #1310).
//
// Each writer is driven with an owner that holds no lease on the row — the one
// precondition all three guards agree on structurally.
func TestLeaseGuardedWritersReturnLeaseSentinel(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// Every case gets its own row: two of the three writers are no-ops on an
	// already-terminal row, so a shared row could hide a guard behind a
	// previous case's transition.
	seed := func(t *testing.T) uuid.UUID {
		t.Helper()
		now := time.Now().UTC()
		holder := uuid.New().String()
		expires := now.Add(5 * time.Minute)
		task := &models.Task{
			ID: uuid.New(), Prompt: "lease sentinel", Status: models.TaskStatusRunning,
			Priority: 10, MaxRetries: 2, CreatedAt: now, StartedAt: &now,
			LeaseOwner: &holder, LeaseExpiresAt: &expires,
		}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return task.ID
	}

	// A stranger holds no lease on any row, so every guard must refuse it.
	stranger := uuid.New()

	for _, tc := range []struct {
		writer string
		call   func(taskID uuid.UUID) error
	}{
		{"storage.UpdateTaskStatusAtomicWithContext", func(id uuid.UUID) error {
			_, err := store.UpdateTaskStatusAtomicWithContext(ctx, id, stranger,
				&models.StatusUpdate{Status: models.TaskStatusRunning})
			return err
		}},
		{"storage.RequeueTaskForRetryWithContext", func(id uuid.UUID) error {
			_, err := store.RequeueTaskForRetryWithContext(ctx, id, stranger,
				time.Now().UTC().Add(time.Minute), "retry backoff")
			return err
		}},
		{"storage.DeadLetterTaskWithContext", func(id uuid.UUID) error {
			_, err := store.DeadLetterTaskWithContext(ctx, id, stranger, "exhausted", 3)
			return err
		}},
	} {
		t.Run(tc.writer, func(t *testing.T) {
			err := tc.call(seed(t))
			if err == nil {
				t.Fatal("writer accepted a caller that holds no lease")
			}
			if !errors.Is(err, ErrTaskLeaseNotHeld) {
				t.Errorf("errors.Is(err, ErrTaskLeaseNotHeld) = false for %v — the lease refusal is not identifiable", err)
			}
			// The operator-facing text is deliberately unchanged by the fix.
			if got := err.Error(); got != ErrTaskLeaseNotHeld.Error() {
				t.Errorf("message = %q, want the sentinel's own text %q", got, ErrTaskLeaseNotHeld.Error())
			}
		})
	}
}
