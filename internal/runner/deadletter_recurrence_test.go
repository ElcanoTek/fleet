package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// seedRecurringTask inserts one PENDING recurring occurrence (@daily, UTC) —
// the shape handleRunFailure's non-retryable path must now spawn a successor
// for (the production incident: one dead-lettered daily run silently ended
// the whole schedule).
func seedRecurringTask(t *testing.T, store *storage.Storage) *models.Task {
	t.Helper()
	task := &models.Task{
		ID:         uuid.New(),
		Prompt:     "daily digest",
		Status:     models.TaskStatusPending,
		Priority:   1,
		Recurrence: "@daily",
		Timezone:   "UTC",
		CreatedAt:  time.Now().UTC(),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	return task
}

// recurrenceSpawnedFlag reads the internal spawn-settlement flag (migration
// 065) straight from the row — it is deliberately not surfaced on models.Task.
func recurrenceSpawnedFlag(t *testing.T, store *storage.Storage, id uuid.UUID) bool {
	t.Helper()
	var settled bool
	if err := store.DB().Conn().QueryRowContext(context.Background(),
		`SELECT recurrence_spawned FROM tasks WHERE id = $1`, id).Scan(&settled); err != nil {
		t.Fatalf("read recurrence_spawned: %v", err)
	}
	return settled
}

// TestNonRetryableFailureOfRecurringTaskSpawnsSuccessor is the runner-level
// proof of the change: a deterministic (non-retryable) failure on a recurring
// occurrence routes to the DLQ through handleRunFailure → sendToDeadLetter →
// storage.DeadLetterTaskWithContext, and that path now spawns the next
// occurrence exactly like a success/error transition — one bad day must not
// silently end a daily schedule.
//
// The seeded occurrence dead-letters and spawns its successor; the pool then
// claims that successor too (ClaimNextPendingTask ignores scheduled_for, and
// the failure runner fails deterministically), so the run ends in the
// quiescent state this test waits for: BOTH occurrences dead-lettered. The
// second dead-letter trips the consecutive-dead-letter breaker (its immediate
// predecessor is dead_lettered), which parks the chain with no third row —
// that fixed point is what makes the assertions deterministic.
func TestNonRetryableFailureOfRecurringTaskSpawnsSuccessor(t *testing.T) {
	store := newTestStore(t)
	orig := seedRecurringTask(t, store)

	pool := NewPool(store, TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		return nil, errors.New("no model configured") // deterministic, not a transient sentinel
	}), Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	// Quiescence: the occurrence dead-letters, its successor is spawned and in
	// turn dead-letters (breaker parks the chain — no third row ever appears).
	waitFor(t, 3*time.Second, func() bool {
		d, _ := store.GetTasksByStatus(models.TaskStatusDeadLettered)
		return len(d) == 2
	})
	cancel()
	<-done

	all, err := store.GetAllTasks()
	if err != nil {
		t.Fatalf("GetAllTasks: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("tasks = %d, want exactly 2 (the dead-lettered occurrence + the one successor it spawned)", len(all))
	}

	var successor *models.Task
	for _, tk := range all {
		if tk.ID == orig.ID {
			continue
		}
		successor = tk
	}
	if successor == nil {
		t.Fatal("no successor row — the non-retryable failure silently ended the recurrence chain")
	}

	gotOrig, err := store.GetTask(orig.ID)
	if err != nil {
		t.Fatalf("GetTask(orig): %v", err)
	}
	if gotOrig.Status != models.TaskStatusDeadLettered {
		t.Errorf("original status = %s, want dead_lettered", gotOrig.Status)
	}
	if gotOrig.DeadLetterReason == nil || !strings.Contains(*gotOrig.DeadLetterReason, "non-retryable") {
		t.Errorf("dead_letter_reason = %v, want a non-retryable failure reason", gotOrig.DeadLetterReason)
	}

	// The successor is a real next occurrence: spawned FROM the dead-lettered
	// row, in its job's lineage, scheduled at the next cron tick.
	if successor.PreviousOccurrenceID == nil || *successor.PreviousOccurrenceID != orig.ID {
		t.Errorf("successor previous_occurrence_id = %v, want %s", successor.PreviousOccurrenceID, orig.ID)
	}
	if successor.LineageID != gotOrig.LineageID {
		t.Errorf("successor lineage_id = %s, want %s", successor.LineageID, gotOrig.LineageID)
	}
	if successor.Recurrence != "@daily" {
		t.Errorf("successor recurrence = %q, want @daily", successor.Recurrence)
	}
	if successor.ScheduledFor == nil || !successor.ScheduledFor.After(time.Now()) {
		t.Errorf("successor scheduled_for = %v, want the next future cron tick", successor.ScheduledFor)
	}
	// And it dead-lettered through the same runner path — its reason records
	// the failure class the operator sees in the DLQ listing.
	if successor.DeadLetterReason == nil || !strings.Contains(*successor.DeadLetterReason, "non-retryable") {
		t.Errorf("successor dead_letter_reason = %v, want a non-retryable failure reason", successor.DeadLetterReason)
	}
	// The breaker settled the parked chain: no third row can be re-driven.
	if !recurrenceSpawnedFlag(t, store, successor.ID) {
		t.Error("the second (breaker-parked) dead-letter must settle the spawn credit")
	}
}
