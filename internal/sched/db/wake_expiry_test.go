package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// parkForWake drives a task through the real running → paused_awaiting_wake
// transition (PauseTaskForWake, which requires the holder's lease), then
// backdates paused_at and overrides wake_at to set up one expiry scenario.
//
// wake_at is written directly rather than passed to PauseTaskForWake for the
// NULL case: PauseTaskForWake always sets it, which is precisely why a NULL
// row is the shape nothing in the system would ever notice.
func parkForWake(t *testing.T, db *Database, prompt string, pausedAgo time.Duration, wakeAt *time.Time) *models.Task {
	t.Helper()
	ctx := context.Background()

	owner := uuid.New()
	ownerStr := owner.String()
	task := &models.Task{ID: uuid.New(), Prompt: prompt, Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.AddTask(ctx, task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	exp := time.Now().Add(5 * time.Minute).UTC()
	task.Status = models.TaskStatusRunning
	task.LeaseOwner = &ownerStr
	task.LeaseExpiresAt = &exp
	if err := db.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask(running): %v", err)
	}
	if ok, err := db.PauseTaskForWake(ctx, task.ID, owner, time.Now().Add(time.Hour).UTC(), "", "sleeping"); err != nil || !ok {
		t.Fatalf("PauseTaskForWake: ok=%v err=%v", ok, err)
	}
	if _, err := db.Conn().ExecContext(ctx,
		`UPDATE tasks SET paused_at = $1, wake_at = $2 WHERE id = $3`,
		time.Now().Add(-pausedAgo).UTC(), wakeAt, task.ID); err != nil {
		t.Fatalf("seed paused_at/wake_at: %v", err)
	}
	return task
}

func ptr(t time.Time) *time.Time { return &t }

// TestExpireStrandedWakeTasks: paused_awaiting_wake had no terminal backstop at
// all — ExpirePausedTasks covers paused_awaiting_input only, and WakeDueTasks
// filters on `wake_at IS NOT NULL`, so a row without one could never wake and
// never fail. It waited forever, silently.
func TestExpireStrandedWakeTasks(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// (1) Unreachable by construction: WakeDueTasks skips a NULL wake_at.
	noWakeAt := parkForWake(t, db, "parked with no deadline", 48*time.Hour, nil)
	// (2) Long overdue: the wake sweep runs every tick, so this far past the
	//     deadline means it is not reaching the row.
	overdue := parkForWake(t, db, "deadline long past", 48*time.Hour, ptr(time.Now().Add(-47*time.Hour).UTC()))
	// (3) A legitimate long sleep: parked days ago, due weeks from now. Must
	//     survive — this is the case a naive "parked too long" rule would break.
	longSleep := parkForWake(t, db, "sleeping until next month", 48*time.Hour, ptr(time.Now().Add(20*24*time.Hour).UTC()))
	// (4) Freshly parked and merely due: the wake sweep gets this one on the
	//     current tick, so expiry must not race it.
	justDue := parkForWake(t, db, "due right now", time.Minute, ptr(time.Now().Add(-time.Minute).UTC()))

	// A non-positive grace disables the sweep entirely.
	if got, err := db.ExpireStrandedWakeTasks(ctx, 0); err != nil || len(got) != 0 {
		t.Fatalf("disabled grace: n=%d err=%v", len(got), err)
	}

	expired, err := db.ExpireStrandedWakeTasks(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("ExpireStrandedWakeTasks: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, e := range expired {
		got[e.ID] = true
	}
	if len(expired) != 2 || !got[noWakeAt.ID] || !got[overdue.ID] {
		t.Fatalf("expired %d task(s) %v; want exactly the NULL-wake_at (%s) and long-overdue (%s) rows",
			len(expired), got, noWakeAt.ID, overdue.ID)
	}

	for _, id := range []uuid.UUID{noWakeAt.ID, overdue.ID} {
		row, _ := db.GetTask(ctx, id)
		if row.Status != models.TaskStatusError || !row.Status.IsTerminal() {
			t.Errorf("task %s status = %s, want a terminal error", id, row.Status)
		}
		if row.CompletedAt == nil {
			t.Errorf("task %s should have completed_at stamped", id)
		}
		if row.ErrorMessage == nil || *row.ErrorMessage == "" {
			t.Errorf("task %s should carry an operator-readable error", id)
		}
		if row.WakeAt != nil {
			t.Errorf("task %s should clear wake_at on the terminal transition, got %v", id, row.WakeAt)
		}
	}

	for _, survivor := range []*models.Task{longSleep, justDue} {
		row, _ := db.GetTask(ctx, survivor.ID)
		if row.Status != models.TaskStatusPausedAwaitingWake {
			t.Errorf("task %q status = %s, want still parked (%s)", survivor.Prompt, row.Status, models.TaskStatusPausedAwaitingWake)
		}
	}

	// Idempotent: a second sweep finds nothing new.
	if got2, err := db.ExpireStrandedWakeTasks(ctx, 24*time.Hour); err != nil || len(got2) != 0 {
		t.Fatalf("second sweep: n=%d err=%v", len(got2), err)
	}
}

// A due row must be WOKEN, not expired. The scheduler orders the wake sweep
// before the expiry sweep for exactly this reason; this locks in that a row the
// wake sweep handles is no longer a candidate.
func TestWakeSweepWinsOverStrandedExpiry(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	due := parkForWake(t, db, "overdue but wakeable", 48*time.Hour, ptr(time.Now().Add(-47*time.Hour).UTC()))

	if n, err := db.WakeDueTasks(ctx); err != nil || n == 0 {
		t.Fatalf("WakeDueTasks: n=%d err=%v", n, err)
	}
	expired, err := db.ExpireStrandedWakeTasks(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("ExpireStrandedWakeTasks: %v", err)
	}
	for _, e := range expired {
		if e.ID == due.ID {
			t.Fatal("a task the wake sweep already re-queued must not then be expired")
		}
	}
	row, _ := db.GetTask(ctx, due.ID)
	if row.Status != models.TaskStatusPending {
		t.Fatalf("woken task status = %s, want pending", row.Status)
	}
}
