package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// makeRunningTask seeds a claimed, running task under owner's lease —
// the state PauseTaskForWake requires.
func makeRunningTask(t *testing.T, db *Database, owner uuid.UUID) *models.Task {
	t.Helper()
	ctx := context.Background()
	ownerStr := owner.String()
	task := &models.Task{ID: uuid.New(), Prompt: "watch the feed", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
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
	return task
}

// Self-wake timer lifecycle: sleep → park (lease released, cycles++) →
// wake sweep re-queues with reason → resumed run clears state under lease,
// cycles survive.
func TestTaskWakeTimerLifecycle(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	owner := uuid.New()
	task := makeRunningTask(t, db, owner)

	// Park requires the holder's lease + running status.
	past := time.Now().Add(-time.Second).UTC()
	if ok, err := db.PauseTaskForWake(ctx, task.ID, uuid.New(), past, "", "note"); err != nil || ok {
		t.Fatalf("wake park with wrong lease must not apply: ok=%v err=%v", ok, err)
	}
	ok, err := db.PauseTaskForWake(ctx, task.ID, owner, past, "", "resume step 3: re-check the feed")
	if err != nil || !ok {
		t.Fatalf("wake park: ok=%v err=%v", ok, err)
	}
	got, _ := db.GetTask(ctx, task.ID)
	if got.Status != models.TaskStatusPausedAwaitingWake {
		t.Fatalf("status = %s; want paused_awaiting_wake", got.Status)
	}
	if got.WakeAt == nil || got.WakeNote != "resume step 3: re-check the feed" || got.WakeCycles != 1 {
		t.Fatalf("wake state not stored: at=%v note=%q cycles=%d", got.WakeAt, got.WakeNote, got.WakeCycles)
	}
	// paused_at (#1116) records the park instant for the wake pause too, so
	// both parked states carry one consistent "entered its pause" timestamp.
	if got.PausedAt == nil {
		t.Fatal("paused_at must be stamped by PauseTaskForWake")
	}
	if got.LeaseOwner != nil || got.LeaseExpiresAt != nil {
		t.Fatal("parked task must hold NO lease (no sandbox)")
	}
	if got.Status.IsTerminal() {
		t.Fatal("paused_awaiting_wake must NOT be terminal (it wakes)")
	}

	// The sweep wakes it (deadline already passed) with the timer reason.
	n, err := db.WakeDueTasks(ctx)
	if err != nil || n != 1 {
		t.Fatalf("WakeDueTasks: n=%d err=%v", n, err)
	}
	got, _ = db.GetTask(ctx, task.ID)
	if got.Status != models.TaskStatusPending || got.WakeReason != "the sleep timer fired" {
		t.Fatalf("after sweep: status=%s reason=%q", got.Status, got.WakeReason)
	}
	if got.WakeNote == "" {
		t.Fatal("wake note must survive the wake (injected into the resumed run)")
	}
	// Sweep is idempotent: nothing left due.
	if n, _ := db.WakeDueTasks(ctx); n != 0 {
		t.Fatalf("second sweep woke %d", n)
	}

	// The woken run claims and clears the wake state at terminal; the cycle
	// counter survives (lifetime cap).
	ownerStr := owner.String()
	got.Status = models.TaskStatusRunning
	got.LeaseOwner = &ownerStr
	_ = db.UpdateTask(ctx, got)
	if err := db.ClearWakeState(ctx, task.ID, owner); err != nil {
		t.Fatalf("ClearWakeState: %v", err)
	}
	got, _ = db.GetTask(ctx, task.ID)
	if got.WakeAt != nil || got.WakeNote != "" || got.WakeReason != "" || got.WakeEventKey != "" {
		t.Fatalf("wake state must clear: %+v", got)
	}
	if got.WakeCycles != 1 {
		t.Fatalf("wake_cycles must survive clearing: %d", got.WakeCycles)
	}
}

// Self-wake event lifecycle: wake_on_event → sweep leaves it parked until the
// timeout → wrong key is a no-op → right key wakes early with the event
// reason; a timed-out event wait names the missing event.
func TestTaskWakeEventLifecycle(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	owner := uuid.New()
	task := makeRunningTask(t, db, owner)

	future := time.Now().Add(time.Hour).UTC()
	if ok, err := db.PauseTaskForWake(ctx, task.ID, owner, future, "deploy-finished", "verify the deploy"); err != nil || !ok {
		t.Fatalf("event park: ok=%v err=%v", ok, err)
	}
	// Not due: the sweep must leave it parked.
	if n, _ := db.WakeDueTasks(ctx); n != 0 {
		t.Fatalf("sweep woke a not-due task: %d", n)
	}
	// Wrong event key: no-op.
	if ok, _ := db.WakeTaskByEvent(ctx, task.ID, "other-event", ""); ok {
		t.Fatal("wrong event key must not wake the task")
	}
	// Right key wakes it early, reason carries the note.
	ok, err := db.WakeTaskByEvent(ctx, task.ID, "deploy-finished", "build 812 green")
	if err != nil || !ok {
		t.Fatalf("event wake: ok=%v err=%v", ok, err)
	}
	got, _ := db.GetTask(ctx, task.ID)
	if got.Status != models.TaskStatusPending {
		t.Fatalf("after event wake: status=%s", got.Status)
	}
	if !strings.Contains(got.WakeReason, `"deploy-finished"`) || !strings.Contains(got.WakeReason, "build 812 green") {
		t.Fatalf("event wake reason: %q", got.WakeReason)
	}
	// A second wake on a non-parked task is a no-op.
	if ok, _ := db.WakeTaskByEvent(ctx, task.ID, "deploy-finished", ""); ok {
		t.Fatal("event wake on a non-parked task must not apply")
	}

	// Timeout path: park again waiting for an event whose deadline passed —
	// the sweep wakes it and the reason names the event that never came.
	ownerStr := owner.String()
	got.Status = models.TaskStatusRunning
	got.LeaseOwner = &ownerStr
	_ = db.UpdateTask(ctx, got)
	past := time.Now().Add(-time.Second).UTC()
	if ok, err := db.PauseTaskForWake(ctx, task.ID, owner, past, "deploy-finished", "verify"); err != nil || !ok {
		t.Fatalf("re-park: ok=%v err=%v", ok, err)
	}
	got, _ = db.GetTask(ctx, task.ID)
	if got.WakeCycles != 2 {
		t.Fatalf("wake_cycles must count every park: %d", got.WakeCycles)
	}
	if n, _ := db.WakeDueTasks(ctx); n != 1 {
		t.Fatal("timeout sweep must wake the task")
	}
	got, _ = db.GetTask(ctx, task.ID)
	if !strings.Contains(got.WakeReason, "timed out") || !strings.Contains(got.WakeReason, `"deploy-finished"`) {
		t.Fatalf("timeout reason: %q", got.WakeReason)
	}
}
