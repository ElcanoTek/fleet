package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// seedExpiredLease adds a task sitting in `status` with an already-expired
// lease and the given attempt/retry counters — the state a crash-looping
// worker leaves behind for RecoverExpiredLeases.
func seedExpiredLease(t *testing.T, store *Storage, status models.TaskStatus, attemptCount, maxRetries int) *models.Task {
	t.Helper()
	owner := uuid.New().String()
	expired := time.Now().Add(-time.Minute).UTC()
	task := &models.Task{
		ID:             uuid.New(),
		Prompt:         "crash loop candidate",
		Status:         status,
		Priority:       10,
		CreatedAt:      time.Now().UTC(),
		LeaseOwner:     &owner,
		LeaseExpiresAt: &expired,
		AttemptCount:   attemptCount,
		MaxRetries:     maxRetries,
	}
	if status == models.TaskStatusRunning {
		started := time.Now().Add(-10 * time.Minute).UTC()
		task.StartedAt = &started
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	return task
}

// TestRecoverExpiredLeasesDeadLettersPastRetryBudget is the crash-loop bound
// (#1116): recovery must NOT re-queue a task whose attempt budget is spent — a
// task that reliably kills the process would otherwise cycle
// recover→claim→crash forever, never reaching the in-process max-retries check
// (a crash never reaches it). The predicate is attempt_count >= max_retries,
// exact parity with the in-process retry gate: max_retries=0 means "never
// retry", so the first crashed attempt quarantines — no free second run of the
// task's external side effects. Quarantine uses the same column shape
// DeadLetterTaskWithContext writes, so the DLQ listing and replay treat it
// like any other quarantined task.
func TestRecoverExpiredLeasesDeadLettersPastRetryBudget(t *testing.T) {
	store, _ := newTestStore(t)

	// attempt_count >= max_retries (0 >= 0, a "never retry" task) → quarantine.
	looping := seedExpiredLease(t, store, models.TaskStatusRunning, 0, 0)
	// attempt_count < max_retries → still within budget: normal re-queue.
	within := seedExpiredLease(t, store, models.TaskStatusLeased, 0, 1)

	requeued, err := store.RecoverExpiredLeases()
	if err != nil {
		t.Fatalf("RecoverExpiredLeases: %v", err)
	}
	if requeued != 1 {
		t.Fatalf("requeued %d, want exactly 1 (the within-budget task; the crash-looper must dead-letter, not re-queue)", requeued)
	}

	got, err := store.GetTask(looping.ID)
	if err != nil {
		t.Fatalf("GetTask(looping): %v", err)
	}
	if got.Status != models.TaskStatusDeadLettered {
		t.Fatalf("crash-looping task status = %s, want dead_lettered (the old code re-queued it forever)", got.Status)
	}
	if got.DeadLetteredAt == nil || got.CompletedAt == nil {
		t.Errorf("quarantined task must read as terminal: dead_lettered_at=%v completed_at=%v", got.DeadLetteredAt, got.CompletedAt)
	}
	if got.DeadLetterAttempts != 1 {
		t.Errorf("dead_letter_attempts = %d, want 1 (attempt_count+1, mirroring the runner's DLQ path)", got.DeadLetterAttempts)
	}
	if got.ActualDurationSeconds == nil {
		t.Error("actual_duration_seconds must be derived from started_at on quarantine, mirroring the runner's terminal writes")
	}
	if got.DeadLetterReason == nil || !strings.Contains(*got.DeadLetterReason, "crash-loop") {
		t.Errorf("dead_letter_reason = %v, want a crash-loop guard reason", got.DeadLetterReason)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage == "" {
		t.Error("error_message must be stamped so the row reads as a terminal failure everywhere")
	}
	if got.LeaseOwner != nil || got.LeaseExpiresAt != nil {
		t.Errorf("lease must be cleared on quarantine: owner=%v expiry=%v", got.LeaseOwner, got.LeaseExpiresAt)
	}

	// The DLQ listing sees it, so `fleet-admin sched dlq list` + replay work
	// on recovery-quarantined rows exactly as on runner-quarantined ones.
	dlq, err := store.GetDeadLetteredTasks(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("GetDeadLetteredTasks: %v", err)
	}
	if len(dlq) != 1 || dlq[0].ID != looping.ID {
		t.Fatalf("DLQ listing = %d row(s), want the quarantined crash-looper", len(dlq))
	}

	gotWithin, err := store.GetTask(within.ID)
	if err != nil {
		t.Fatalf("GetTask(within): %v", err)
	}
	if gotWithin.Status != models.TaskStatusPending {
		t.Errorf("within-budget task status = %s, want pending (normal recovery)", gotWithin.Status)
	}
	if gotWithin.AttemptCount != 1 {
		t.Errorf("within-budget attempt_count = %d, want 1 (recovery increments)", gotWithin.AttemptCount)
	}
}

// TestRecoverExpiredLeasesBoundsACrashLoop drives the full cycle: a task with
// max_retries=1 whose lease expires again and again (the worker "crashes"
// every attempt) must be re-queued a bounded number of times and then land in
// the DLQ — never cycle forever. Parity with the in-process gate: max_retries=R
// allows at most R+1 total executions.
func TestRecoverExpiredLeasesBoundsACrashLoop(t *testing.T) {
	store, _ := newTestStore(t)
	task := seedExpiredLease(t, store, models.TaskStatusRunning, 0, 1)

	release := func() {
		t.Helper()
		owner := uuid.New().String()
		expired := time.Now().Add(-time.Minute).UTC()
		if _, err := store.DB().Conn().ExecContext(context.Background(), `
			UPDATE tasks SET status = $1, lease_owner = $2, lease_expires_at = $3 WHERE id = $4`,
			string(models.TaskStatusRunning), owner, expired, task.ID); err != nil {
			t.Fatalf("re-lease: %v", err)
		}
	}

	for cycle := 0; cycle < 10; cycle++ {
		if _, err := store.RecoverExpiredLeases(); err != nil {
			t.Fatalf("cycle %d: RecoverExpiredLeases: %v", cycle, err)
		}
		got, err := store.GetTask(task.ID)
		if err != nil {
			t.Fatalf("cycle %d: GetTask: %v", cycle, err)
		}
		if got.Status == models.TaskStatusDeadLettered {
			// Bounded at R+1 = 2 executions: recovery cycle 0 re-queues
			// (attempt_count 0 < 1, counter walks to 1); cycle 1 quarantines
			// (attempt_count 1 >= 1) — the second crashed run was the last
			// allowed one.
			if cycle != 1 {
				t.Errorf("dead-lettered on recovery cycle %d, want cycle 1 (attempt budget = max_retries+1 executions)", cycle)
			}
			if got.DeadLetterAttempts != 2 {
				t.Errorf("dead_letter_attempts = %d, want 2 (both allowed executions were made)", got.DeadLetterAttempts)
			}
			return
		}
		if got.Status != models.TaskStatusPending {
			t.Fatalf("cycle %d: status = %s, want pending or dead_lettered", cycle, got.Status)
		}
		release() // the "fresh claim" crashes again: back to an expired running lease
	}
	t.Fatal("crash loop never dead-lettered after 10 recovery cycles — the retry budget did not bound it")
}
