package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestRunIfColumnsRoundTrip pins the persistence of the run_if + skip columns
// (#269): a task carrying run_if / skip_count / last_skip_at / last_skip_reason
// round-trips through AddTask + GetTask, and a plain task reads them back
// nil/zero (the legacy unconditional promotion path).
func TestRunIfColumnsRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	plain := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, plain); err != nil {
		t.Fatalf("add plain: %v", err)
	}
	got, err := store.GetTask(plain.ID)
	if err != nil {
		t.Fatalf("get plain: %v", err)
	}
	if got.RunIf != nil || got.SkipCount != 0 || got.LastSkipAt != nil || got.LastSkipReason != nil {
		t.Errorf("plain task must have empty run_if/skip columns; got run_if=%v skip_count=%d last_skip_at=%v reason=%v",
			got.RunIf, got.SkipCount, got.LastSkipAt, got.LastSkipReason)
	}

	gate := &models.RunIf{Command: "false", ExitCodeIs: 0, TimeoutSeconds: 30, OnError: models.RunIfOnErrorSkip}
	at := time.Now().UTC().Truncate(time.Microsecond)
	reason := "exit 1 (want 0): "
	gated := &models.Task{
		ID: uuid.New(), Prompt: "p", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		Recurrence: "@daily", RunIf: gate, SkipCount: 2, LastSkipAt: &at, LastSkipReason: &reason,
	}
	if _, err := store.AddTaskWithContext(ctx, gated); err != nil {
		t.Fatalf("add gated: %v", err)
	}
	got, err = store.GetTask(gated.ID)
	if err != nil {
		t.Fatalf("get gated: %v", err)
	}
	if got.RunIf == nil {
		t.Fatal("run_if must round-trip non-nil")
	}
	if got.RunIf.Command != "false" || got.RunIf.ExitCodeIs != 0 || got.RunIf.TimeoutSeconds != 30 || got.RunIf.OnError != models.RunIfOnErrorSkip {
		t.Errorf("run_if fields did not round-trip: %+v", got.RunIf)
	}
	if got.SkipCount != 2 {
		t.Errorf("skip_count = %d, want 2", got.SkipCount)
	}
	if got.LastSkipAt == nil || !got.LastSkipAt.Equal(at) {
		t.Errorf("last_skip_at = %v, want %v", got.LastSkipAt, at)
	}
	if got.LastSkipReason == nil || *got.LastSkipReason != reason {
		t.Errorf("last_skip_reason = %v, want %q", got.LastSkipReason, reason)
	}
}

// TestRecordSkipAdvancesScheduledFor pins the skip path (#269): a still-
// scheduled task's scheduled_for advances to nextRun, skip_count increments,
// and last_skip_at + last_skip_reason are stamped — status stays scheduled.
func TestRecordSkipAdvancesScheduledFor(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-1 * time.Hour)
	task := &models.Task{
		ID: uuid.New(), Prompt: "p", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		Recurrence: "@daily", ScheduledFor: &past, RunIf: &models.RunIf{Command: "false", TimeoutSeconds: 5},
	}
	if _, err := store.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("add: %v", err)
	}

	next := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	got, recorded, err := store.RecordSkip(ctx, task.ID, "exit 1 (want 0)", next, &past)
	if err != nil {
		t.Fatalf("record skip: %v", err)
	}
	if !recorded {
		t.Fatal("recorded = false, want true (row unchanged since dispatch)")
	}
	if got.Status != models.TaskStatusScheduled {
		t.Errorf("status = %s, want scheduled (skip must not promote)", got.Status)
	}
	if got.SkipCount != 1 {
		t.Errorf("skip_count = %d, want 1", got.SkipCount)
	}
	if got.LastSkipAt == nil {
		t.Error("last_skip_at must be stamped")
	}
	if got.LastSkipReason == nil || *got.LastSkipReason != "exit 1 (want 0)" {
		t.Errorf("last_skip_reason = %v, want the reason", got.LastSkipReason)
	}
	if got.ScheduledFor == nil || !got.ScheduledFor.Equal(next) {
		t.Errorf("scheduled_for = %v, want %v", got.ScheduledFor, next)
	}

	// A second skip increments skip_count to 2. The observed scheduled_for is
	// the one the first skip advanced the row to (a re-dispatch always carries
	// the freshly fetched value).
	got, recorded, err = store.RecordSkip(ctx, task.ID, "exit 1 (want 0)", next.Add(24*time.Hour), &next)
	if err != nil {
		t.Fatalf("record skip 2: %v", err)
	}
	if !recorded {
		t.Fatal("recorded = false on the second skip, want true")
	}
	if got.SkipCount != 2 {
		t.Errorf("skip_count = %d, want 2", got.SkipCount)
	}
}

// TestRecordSkipNoopAfterPromotion pins the guard: once a task has left the
// scheduled state (e.g. it was promoted to pending, cancelled, or claimed), a
// concurrent RecordSkip is a no-op rather than resurrecting a stale skip.
func TestRecordSkipNoopAfterPromotion(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Microsecond)
	task := &models.Task{
		ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(),
		Recurrence: "@daily", ScheduledFor: &past, RunIf: &models.RunIf{Command: "false", TimeoutSeconds: 5},
	}
	if _, err := store.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("add: %v", err)
	}

	next := time.Now().UTC().Add(24 * time.Hour)
	got, recorded, err := store.RecordSkip(ctx, task.ID, "exit 1", next, &past)
	if err != nil {
		t.Fatalf("record skip on pending task: %v", err)
	}
	if recorded {
		t.Error("recorded = true, want false (task left scheduled)")
	}
	if got.SkipCount != 0 {
		t.Errorf("skip_count = %d, want 0 (skip must be a no-op off scheduled)", got.SkipCount)
	}
	if got.ScheduledFor == nil || !got.ScheduledFor.Equal(past) {
		t.Errorf("scheduled_for = %v, want unchanged %v", got.ScheduledFor, past)
	}
}

// TestRecordSkipNoopAfterReschedule pins the async-settle guard: a task edited
// or postponed while its gate evaluation was in flight (status stays
// `scheduled`, but scheduled_for moved) must NOT have the stale verdict's
// backoff clobber the operator's new scheduled_for. The skip is a no-op and
// the next due tick re-evaluates the current definition.
func TestRecordSkipNoopAfterReschedule(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Microsecond)
	task := &models.Task{
		ID: uuid.New(), Prompt: "p", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		ScheduledFor: &past, RunIf: &models.RunIf{Command: "false", TimeoutSeconds: 5},
	}
	if _, err := store.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("add: %v", err)
	}

	// An operator postpones the task while the gate runs.
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Microsecond)
	if _, err := store.DB().Conn().ExecContext(ctx,
		"UPDATE tasks SET scheduled_for = $1 WHERE id = $2", tomorrow, task.ID); err != nil {
		t.Fatalf("reschedule: %v", err)
	}

	// The stale verdict settles, still carrying the scheduled_for it was
	// dispatched against.
	got, recorded, err := store.RecordSkip(ctx, task.ID, "exit 1", time.Now().UTC().Add(time.Minute), &past)
	if err != nil {
		t.Fatalf("record skip after reschedule: %v", err)
	}
	if recorded {
		t.Error("recorded = true, want false (the interleaved reschedule must win)")
	}
	if got.SkipCount != 0 {
		t.Errorf("skip_count = %d, want 0", got.SkipCount)
	}
	if got.ScheduledFor == nil || !got.ScheduledFor.Equal(tomorrow) {
		t.Errorf("scheduled_for = %v, want the operator's %v kept", got.ScheduledFor, tomorrow)
	}
}

// TestRecordSkipOneShot verifies that skipping a one-shot (non-recurring) task
// increments the skip count and sets the last skip fields, but leaves its
// scheduled_for timestamp unchanged (without advancing it).
func TestRecordSkipOneShot(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Microsecond)
	task := &models.Task{
		ID: uuid.New(), Prompt: "p", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		Recurrence: "", ScheduledFor: &past, RunIf: &models.RunIf{Command: "false", TimeoutSeconds: 5},
	}
	if _, err := store.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("add: %v", err)
	}

	// For a one-shot task, we pass a zero nextRun time.
	got, recorded, err := store.RecordSkip(ctx, task.ID, "exit 1 (want 0)", time.Time{}, &past)
	if err != nil {
		t.Fatalf("record skip on one-shot: %v", err)
	}
	if !recorded {
		t.Fatal("recorded = false, want true")
	}
	if got.Status != models.TaskStatusScheduled {
		t.Errorf("status = %s, want scheduled", got.Status)
	}
	if got.SkipCount != 1 {
		t.Errorf("skip_count = %d, want 1", got.SkipCount)
	}
	if got.LastSkipAt == nil {
		t.Error("last_skip_at must be stamped")
	}
	if got.LastSkipReason == nil || *got.LastSkipReason != "exit 1 (want 0)" {
		t.Errorf("last_skip_reason = %v, want the reason", got.LastSkipReason)
	}
	// scheduled_for must remain unchanged (still equal to past).
	if got.ScheduledFor == nil || !got.ScheduledFor.Equal(past) {
		t.Errorf("scheduled_for = %v, want unchanged %v", got.ScheduledFor, past)
	}
}

// TestComputeNextRun pins the cron evaluation used by the skip path (#269):
// the next occurrence is computed in the task's own timezone and returned as a
// UTC instant in the future.
func TestComputeNextRun(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")

	task := &models.Task{
		ID: uuid.New(), Prompt: "p", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		Recurrence: "@hourly", Timezone: "UTC",
	}
	next, err := store.ComputeNextRun(task)
	if err != nil {
		t.Fatalf("compute next run: %v", err)
	}
	if !next.After(time.Now().UTC()) {
		t.Errorf("next run must be in the future, got %v", next)
	}
	if next.Location() != time.UTC {
		t.Errorf("next run must be UTC, got %v", next.Location())
	}

	// A bogus recurrence surfaces a parse error rather than panicking.
	task.Recurrence = "not a cron"
	if _, err := store.ComputeNextRun(task); err == nil {
		t.Error("compute next run with a bogus recurrence must error")
	}
}
