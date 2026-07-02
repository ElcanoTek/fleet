package scheduler

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// runWithLivelockGuard runs ProcessScheduledTasks on another goroutine and
// fails the test if it does not return within a generous deadline — the #566
// failure mode is an infinite loop, so "the function returns at all" is the
// assertion.
func runWithLivelockGuard(t *testing.T, s *Scheduler) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ProcessScheduledTasks()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("ProcessScheduledTasks did not return: live-lock (#566) — a full batch of non-promotable tasks must still terminate the loop")
	}
}

// pastDue returns a scheduled_for instant safely in the past.
func pastDue() *time.Time {
	ts := time.Now().UTC().Add(-time.Minute)
	return &ts
}

// decliningGate returns a run_if gate that always declines (soft hold): `false`
// exits 1, ExitCodeIs is 0.
func decliningGate() *models.RunIf {
	return &models.RunIf{Command: "false", ExitCodeIs: 0, TimeoutSeconds: 5}
}

// TestProcessScheduledTasksAllSoftHeldTerminates reproduces issue #566: a FULL
// batch of due one-shot tasks whose run_if gates all decline used to make zero
// forward progress (soft-held one-shots keep their scheduled_for, so the same
// rows are re-fetched) while `len(tasks) == batchSize` kept the loop going —
// an infinite loop that hung the scheduler goroutine, and with it lease
// recovery and starvation promotion for the whole box.
func TestProcessScheduledTasksAllSoftHeldTerminates(t *testing.T) {
	s, store := newTestScheduler(t)
	s.scheduledBatchSize = 3 // a FULL batch of soft-held tasks with 3 rows

	ids := make([]uuid.UUID, 0, 3)
	for range 3 {
		task := &models.Task{
			ID:           uuid.New(),
			Prompt:       "soft-held one-shot",
			Status:       models.TaskStatusScheduled,
			ScheduledFor: pastDue(),
			RunIf:        decliningGate(),
			CreatedAt:    time.Now().UTC(),
		}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		ids = append(ids, task.ID)
	}

	runWithLivelockGuard(t, s)

	// The soft hold semantics are preserved: still scheduled, still due, skip
	// recorded exactly once (the loop must break after the first zero-progress
	// pass, not re-skip the same rows N times).
	for _, id := range ids {
		task, err := store.GetTask(id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", id, err)
		}
		if task.Status != models.TaskStatusScheduled {
			t.Errorf("task %s: soft-held one-shot must stay scheduled, got %s", id, task.Status)
		}
		if task.SkipCount != 1 {
			t.Errorf("task %s: want exactly 1 recorded skip for the tick, got %d", id, task.SkipCount)
		}
	}
}

// TestProcessScheduledTasksMixedBatchPromotesAndTerminates: a full batch mixing
// promotable, soft-held one-shot, and recurring-declined tasks must promote the
// promotable ones, advance the recurring one, soft-hold the one-shot, and
// return.
func TestProcessScheduledTasksMixedBatchPromotesAndTerminates(t *testing.T) {
	s, store := newTestScheduler(t)
	s.scheduledBatchSize = 4

	plain1 := &models.Task{ID: uuid.New(), Prompt: "promotable A", Status: models.TaskStatusScheduled, ScheduledFor: pastDue(), CreatedAt: time.Now().UTC()}
	plain2 := &models.Task{ID: uuid.New(), Prompt: "promotable B", Status: models.TaskStatusScheduled, ScheduledFor: pastDue(), CreatedAt: time.Now().UTC()}
	oneShotHeld := &models.Task{ID: uuid.New(), Prompt: "held one-shot", Status: models.TaskStatusScheduled, ScheduledFor: pastDue(), RunIf: decliningGate(), CreatedAt: time.Now().UTC()}
	recurringHeld := &models.Task{ID: uuid.New(), Prompt: "held recurring", Status: models.TaskStatusScheduled, ScheduledFor: pastDue(), RunIf: decliningGate(), Recurrence: "@daily", Timezone: "UTC", CreatedAt: time.Now().UTC()}
	for _, task := range []*models.Task{plain1, plain2, oneShotHeld, recurringHeld} {
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
	}

	runWithLivelockGuard(t, s)

	for _, id := range []uuid.UUID{plain1.ID, plain2.ID} {
		task, err := store.GetTask(id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", id, err)
		}
		if task.Status != models.TaskStatusPending {
			t.Errorf("promotable task %s: want pending, got %s", id, task.Status)
		}
	}

	held, err := store.GetTask(oneShotHeld.ID)
	if err != nil {
		t.Fatalf("GetTask(held one-shot): %v", err)
	}
	if held.Status != models.TaskStatusScheduled || held.SkipCount != 1 {
		t.Errorf("held one-shot: want scheduled with 1 skip, got status=%s skips=%d", held.Status, held.SkipCount)
	}

	rec, err := store.GetTask(recurringHeld.ID)
	if err != nil {
		t.Fatalf("GetTask(held recurring): %v", err)
	}
	if rec.Status != models.TaskStatusScheduled {
		t.Errorf("held recurring: want scheduled, got %s", rec.Status)
	}
	if rec.ScheduledFor == nil || !rec.ScheduledFor.After(time.Now()) {
		t.Errorf("held recurring: scheduled_for must advance to the next cron tick, got %v", rec.ScheduledFor)
	}
}

// TestProcessScheduledTasksHeldPrefixDoesNotStarve: soft-held one-shots sort
// FIRST (oldest scheduled_for) and stay due, so a naive zero-progress break
// after the first all-held batch would leave promotable tasks behind them
// unreached. The widening fetch window must page past the held prefix and
// promote them in the same tick.
func TestProcessScheduledTasksHeldPrefixDoesNotStarve(t *testing.T) {
	s, store := newTestScheduler(t)
	s.scheduledBatchSize = 2

	// Two held one-shots due 2h ago — they fill the entire first batch.
	early := time.Now().UTC().Add(-2 * time.Hour)
	heldIDs := make([]uuid.UUID, 0, 2)
	for i := range 2 {
		ts := early.Add(time.Duration(i) * time.Minute)
		task := &models.Task{ID: uuid.New(), Prompt: "held", Status: models.TaskStatusScheduled, ScheduledFor: &ts, RunIf: decliningGate(), CreatedAt: time.Now().UTC()}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		heldIDs = append(heldIDs, task.ID)
	}
	// Two promotable tasks due later (but still past) — behind the held prefix.
	promotableIDs := make([]uuid.UUID, 0, 2)
	for i := range 2 {
		ts := time.Now().UTC().Add(-time.Duration(10-i) * time.Minute)
		task := &models.Task{ID: uuid.New(), Prompt: "promotable behind held prefix", Status: models.TaskStatusScheduled, ScheduledFor: &ts, CreatedAt: time.Now().UTC()}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		promotableIDs = append(promotableIDs, task.ID)
	}

	runWithLivelockGuard(t, s)

	for _, id := range promotableIDs {
		task, err := store.GetTask(id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", id, err)
		}
		if task.Status != models.TaskStatusPending {
			t.Errorf("promotable task %s behind the held prefix: want pending, got %s", id, task.Status)
		}
	}
	for _, id := range heldIDs {
		task, err := store.GetTask(id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", id, err)
		}
		if task.Status != models.TaskStatusScheduled || task.SkipCount != 1 {
			t.Errorf("held task %s: want scheduled with exactly 1 skip, got status=%s skips=%d", id, task.Status, task.SkipCount)
		}
	}
}

// TestProcessScheduledTasksTiedScheduledForNotMasked: scheduled_for alone is
// not a total order. With plain LIMIT paging, rows tied at the page boundary
// have no stable position across queries, so a due row could be masked for the
// whole tick. The keyset cursor's id tiebreaker must walk EVERY row of a large
// tied group exactly once — held ones skipped once each, promotable ones
// promoted — across multiple pages.
func TestProcessScheduledTasksTiedScheduledForNotMasked(t *testing.T) {
	s, store := newTestScheduler(t)
	s.scheduledBatchSize = 2

	tied := time.Now().UTC().Add(-time.Hour) // one instant shared by ALL rows
	heldIDs := make([]uuid.UUID, 0, 3)
	promotableIDs := make([]uuid.UUID, 0, 3)
	for i := range 6 {
		task := &models.Task{ID: uuid.New(), Prompt: "tied", Status: models.TaskStatusScheduled, ScheduledFor: &tied, CreatedAt: time.Now().UTC()}
		if i%2 == 0 {
			task.RunIf = decliningGate()
		}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		if task.RunIf != nil {
			heldIDs = append(heldIDs, task.ID)
		} else {
			promotableIDs = append(promotableIDs, task.ID)
		}
	}

	runWithLivelockGuard(t, s)

	for _, id := range promotableIDs {
		task, err := store.GetTask(id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", id, err)
		}
		if task.Status != models.TaskStatusPending {
			t.Errorf("tied promotable task %s: want pending, got %s", id, task.Status)
		}
	}
	for _, id := range heldIDs {
		task, err := store.GetTask(id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", id, err)
		}
		if task.Status != models.TaskStatusScheduled || task.SkipCount != 1 {
			t.Errorf("tied held task %s: want scheduled with exactly 1 skip, got status=%s skips=%d", id, task.Status, task.SkipCount)
		}
	}
}

// TestProcessScheduledTasksPagesThroughFullBatches: forward progress must keep
// the loop paging — with batchSize 2 and 5 promotable due tasks, all 5 end up
// pending in one call (the fix must not turn the pagination into a single-batch
// pass).
func TestProcessScheduledTasksPagesThroughFullBatches(t *testing.T) {
	s, store := newTestScheduler(t)
	s.scheduledBatchSize = 2

	ids := make([]uuid.UUID, 0, 5)
	for range 5 {
		task := &models.Task{
			ID:           uuid.New(),
			Prompt:       "promotable",
			Status:       models.TaskStatusScheduled,
			ScheduledFor: pastDue(),
			CreatedAt:    time.Now().UTC(),
		}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		ids = append(ids, task.ID)
	}

	runWithLivelockGuard(t, s)

	for _, id := range ids {
		task, err := store.GetTask(id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", id, err)
		}
		if task.Status != models.TaskStatusPending {
			t.Errorf("task %s: want pending after paging, got %s", id, task.Status)
		}
	}
}
